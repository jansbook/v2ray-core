package shadowsocks

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha1"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"v2ray.com/core/common"
	"v2ray.com/core/common/buf"
	"v2ray.com/core/common/crypto"
	"v2ray.com/core/common/protocol"
)

type ShadowsocksAccount struct {
	Cipher      Cipher
	Key         []byte
	OneTimeAuth Account_OneTimeAuth
}

func (v *ShadowsocksAccount) Equals(another protocol.Account) bool {
	if account, ok := another.(*ShadowsocksAccount); ok {
		return bytes.Equal(v.Key, account.Key)
	}
	return false
}

func createAesGcm(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	common.Must(err)
	gcm, err := cipher.NewGCM(block)
	common.Must(err)
	return gcm
}

func createChacha20Poly1305(key []byte) cipher.AEAD {
	chacha20, err := chacha20poly1305.New(key)
	common.Must(err)
	return chacha20
}

func (v *Account) GetCipher() (Cipher, error) {
	switch v.CipherType {
	case CipherType_AES_128_CFB:
		return &AesCfb{KeyBytes: 16}, nil
	case CipherType_AES_256_CFB:
		return &AesCfb{KeyBytes: 32}, nil
	case CipherType_CHACHA20:
		return &ChaCha20{IVBytes: 8}, nil
	case CipherType_CHACHA20_IETF:
		return &ChaCha20{IVBytes: 12}, nil
	case CipherType_AES_128_GCM:
		return &AEADCipher{
			KeyBytes:        16,
			IVBytes:         16,
			AEADAuthCreator: createAesGcm,
		}, nil
	case CipherType_AES_256_GCM:
		return &AEADCipher{
			KeyBytes:        32,
			IVBytes:         32,
			AEADAuthCreator: createAesGcm,
		}, nil
	case CipherType_CHACHA20_POLY1305:
		return &AEADCipher{
			KeyBytes:        32,
			IVBytes:         32,
			AEADAuthCreator: createChacha20Poly1305,
		}, nil
	default:
		return nil, newError("Unsupported cipher.")
	}
}

func (v *Account) AsAccount() (protocol.Account, error) {
	cipher, err := v.GetCipher()
	if err != nil {
		return nil, newError("failed to get cipher").Base(err)
	}
	return &ShadowsocksAccount{
		Cipher:      cipher,
		Key:         v.GetCipherKey(),
		OneTimeAuth: v.Ota,
	}, nil
}

func (v *Account) GetCipherKey() []byte {
	ct, err := v.GetCipher()
	if err != nil {
		return nil
	}
	return PasswordToCipherKey(v.Password, ct.KeySize())
}

type Cipher interface {
	KeySize() int
	IVSize() int
	NewEncryptionWriter(key []byte, iv []byte, writer io.Writer) (buf.Writer, error)
	NewDecryptionReader(key []byte, iv []byte, reader io.Reader) (buf.Reader, error)
	IsAEAD() bool
	EncodePacket(key []byte, b *buf.Buffer) error
	DecodePacket(key []byte, b *buf.Buffer) error
}

type AesCfb struct {
	KeyBytes int
}

func (*AesCfb) IsAEAD() bool {
	return false
}

func (v *AesCfb) KeySize() int {
	return v.KeyBytes
}

func (v *AesCfb) IVSize() int {
	return 16
}

func (v *AesCfb) NewEncryptionWriter(key []byte, iv []byte, writer io.Writer) (buf.Writer, error) {
	stream := crypto.NewAesEncryptionStream(key, iv)
	return buf.NewWriter(crypto.NewCryptionWriter(stream, writer)), nil
}

func (v *AesCfb) NewDecryptionReader(key []byte, iv []byte, reader io.Reader) (buf.Reader, error) {
	stream := crypto.NewAesDecryptionStream(key, iv)
	return buf.NewReader(crypto.NewCryptionReader(stream, reader)), nil
}

func (v *AesCfb) EncodePacket(key []byte, b *buf.Buffer) error {
	iv := b.BytesTo(v.IVSize())
	stream := crypto.NewAesEncryptionStream(key, iv)
	stream.XORKeyStream(b.BytesFrom(v.IVSize()), b.BytesFrom(v.IVSize()))
	return nil
}

func (v *AesCfb) DecodePacket(key []byte, b *buf.Buffer) error {
	iv := b.BytesTo(v.IVSize())
	stream := crypto.NewAesDecryptionStream(key, iv)
	stream.XORKeyStream(b.BytesFrom(v.IVSize()), b.BytesFrom(v.IVSize()))
	b.SliceFrom(v.IVSize())
	return nil
}

type AEADCipher struct {
	KeyBytes        int
	IVBytes         int
	AEADAuthCreator func(key []byte) cipher.AEAD
}

func (*AEADCipher) IsAEAD() bool {
	return true
}

func (c *AEADCipher) KeySize() int {
	return c.KeyBytes
}

func (c *AEADCipher) IVSize() int {
	return c.IVBytes
}

func (c *AEADCipher) createAuthenticator(key []byte, iv []byte) *crypto.AEADAuthenticator {
	nonce := crypto.NewIncreasingAEADNonceGenerator()
	subkey := make([]byte, c.KeyBytes)
	hkdfSHA1(key, iv, subkey)
	return &crypto.AEADAuthenticator{
		AEAD:           c.AEADAuthCreator(subkey),
		NonceGenerator: nonce,
	}
}

func (c *AEADCipher) NewEncryptionWriter(key []byte, iv []byte, writer io.Writer) (buf.Writer, error) {
	auth := c.createAuthenticator(key, iv)
	return crypto.NewAuthenticationWriter(auth, &crypto.AEADChunkSizeParser{
		Auth: auth,
	}, writer, protocol.TransferTypeStream), nil
}

func (c *AEADCipher) NewDecryptionReader(key []byte, iv []byte, reader io.Reader) (buf.Reader, error) {
	auth := c.createAuthenticator(key, iv)
	return crypto.NewAuthenticationReader(auth, &crypto.AEADChunkSizeParser{
		Auth: auth,
	}, reader, protocol.TransferTypeStream), nil
}

func (c *AEADCipher) EncodePacket(key []byte, b *buf.Buffer) error {
	ivLen := c.IVSize()
	payloadLen := b.Len()
	auth := c.createAuthenticator(key, b.BytesTo(ivLen))
	return b.Reset(func(bb []byte) (int, error) {
		bbb, err := auth.Seal(bb[:ivLen], bb[ivLen:payloadLen])
		if err != nil {
			return 0, err
		}
		return len(bbb), nil
	})
}

func (c *AEADCipher) DecodePacket(key []byte, b *buf.Buffer) error {
	ivLen := c.IVSize()
	payloadLen := b.Len()
	auth := c.createAuthenticator(key, b.BytesTo(ivLen))
	if err := b.Reset(func(bb []byte) (int, error) {
		bbb, err := auth.Open(bb[:ivLen], bb[ivLen:payloadLen])
		if err != nil {
			return 0, err
		}
		return len(bbb), nil
	}); err != nil {
		return err
	}
	b.SliceFrom(ivLen)
	return nil
}

type ChaCha20 struct {
	IVBytes int
}

func (*ChaCha20) IsAEAD() bool {
	return false
}

func (v *ChaCha20) KeySize() int {
	return 32
}

func (v *ChaCha20) IVSize() int {
	return v.IVBytes
}

func (v *ChaCha20) NewEncryptionWriter(key []byte, iv []byte, writer io.Writer) (buf.Writer, error) {
	stream := crypto.NewChaCha20Stream(key, iv)
	return buf.NewWriter(crypto.NewCryptionWriter(stream, writer)), nil
}

func (v *ChaCha20) NewDecryptionReader(key []byte, iv []byte, reader io.Reader) (buf.Reader, error) {
	stream := crypto.NewChaCha20Stream(key, iv)
	return buf.NewReader(crypto.NewCryptionReader(stream, reader)), nil
}

func (v *ChaCha20) EncodePacket(key []byte, b *buf.Buffer) error {
	iv := b.BytesTo(v.IVSize())
	stream := crypto.NewChaCha20Stream(key, iv)
	stream.XORKeyStream(b.BytesFrom(v.IVSize()), b.BytesFrom(v.IVSize()))
	return nil
}

func (v *ChaCha20) DecodePacket(key []byte, b *buf.Buffer) error {
	iv := b.BytesTo(v.IVSize())
	stream := crypto.NewChaCha20Stream(key, iv)
	stream.XORKeyStream(b.BytesFrom(v.IVSize()), b.BytesFrom(v.IVSize()))
	b.SliceFrom(v.IVSize())
	return nil
}

func PasswordToCipherKey(password string, keySize int) []byte {
	pwdBytes := []byte(password)
	key := make([]byte, 0, keySize)

	md5Sum := md5.Sum(pwdBytes)
	key = append(key, md5Sum[:]...)

	for len(key) < keySize {
		md5Hash := md5.New()
		md5Hash.Write(md5Sum[:])
		md5Hash.Write(pwdBytes)
		md5Hash.Sum(md5Sum[:0])

		key = append(key, md5Sum[:]...)
	}
	return key
}

func hkdfSHA1(secret, salt, outkey []byte) {
	r := hkdf.New(sha1.New, secret, salt, []byte("ss-subkey"))
	common.Must2(io.ReadFull(r, outkey))
}
