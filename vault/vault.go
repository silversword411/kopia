package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/kopia/kopia/blob"
	"github.com/kopia/kopia/repo"

	"golang.org/x/crypto/hkdf"
)

const (
	formatBlockID         = "format"
	checksumBlockID       = "checksum"
	repositoryConfigBlock = "repo"
)

var (
	purposeAESKey         = []byte("AES")
	purposeChecksumSecret = []byte("CHECKSUM")
)

// ErrItemNotFound is an error returned when a vault item cannot be found.
var ErrItemNotFound = errors.New("item not found")

// Vault is a secure storage for the secrets.
type Vault struct {
	storage   blob.Storage
	masterKey []byte
	format    Format
}

type repositoryConfig struct {
	Connection blob.ConnectionInfo `json:"connection"`
	Format     *repo.Format        `json:"format"`
}

// Put saves the specified content in a vault under a specified name.
func (v *Vault) Put(itemID string, content []byte) error {
	blk, err := v.newCipher()
	if err != nil {
		return err
	}

	if blk != nil {
		hash, err := v.newChecksum()
		if err != nil {
			return err
		}

		ivLength := blk.BlockSize()
		ivPlusContentLength := ivLength + len(content)
		cipherText := make([]byte, ivPlusContentLength+hash.Size())

		// Store IV at the beginning of ciphertext.
		iv := cipherText[0:ivLength]
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return err
		}

		ctr := cipher.NewCTR(blk, iv)
		ctr.XORKeyStream(cipherText[ivLength:], content)
		hash.Write(cipherText[0:ivPlusContentLength])
		copy(cipherText[ivPlusContentLength:], hash.Sum(nil))

		content = cipherText
	}

	return v.storage.PutBlock(itemID, blob.NewReader(bytes.NewBuffer(content)), blob.PutOptionsOverwrite)
}

func (v *Vault) readEncryptedBlock(itemID string) ([]byte, error) {
	content, err := v.storage.GetBlock(itemID)
	if err != nil {
		return nil, err
	}

	blk, err := v.newCipher()
	if err != nil {
		return nil, err
	}

	if blk != nil {

		hash, err := v.newChecksum()
		if err != nil {
			return nil, err
		}

		p := len(content) - hash.Size()
		hash.Write(content[0:p])
		expectedChecksum := hash.Sum(nil)
		actualChecksum := content[p:]
		if !hmac.Equal(expectedChecksum, actualChecksum) {
			return nil, fmt.Errorf("cannot read encrypted block: incorrect checksum")
		}

		ivLength := blk.BlockSize()

		plainText := make([]byte, len(content)-ivLength-hash.Size())
		iv := content[0:blk.BlockSize()]

		ctr := cipher.NewCTR(blk, iv)
		ctr.XORKeyStream(plainText, content[ivLength:len(content)-hash.Size()])

		content = plainText
	}

	return content, nil
}

func (v *Vault) newChecksum() (hash.Hash, error) {
	switch v.format.Checksum {
	case "hmac-sha-256":
		key := make([]byte, 32)
		v.deriveKey(purposeChecksumSecret, key)
		return hmac.New(sha256.New, key), nil

	default:
		return nil, fmt.Errorf("unsupported checksum format: %v", v.format.Checksum)
	}

}

func (v *Vault) newCipher() (cipher.Block, error) {
	switch v.format.Encryption {
	case "none":
		return nil, nil
	case "aes-128":
		k := make([]byte, 16)
		v.deriveKey(purposeAESKey, k)
		return aes.NewCipher(k)
	case "aes-192":
		k := make([]byte, 24)
		v.deriveKey(purposeAESKey, k)
		return aes.NewCipher(k)
	case "aes-256":
		k := make([]byte, 32)
		v.deriveKey(purposeAESKey, k)
		return aes.NewCipher(k)
	default:
		return nil, fmt.Errorf("unsupported encryption format: %v", v.format.Encryption)
	}

}

func (v *Vault) deriveKey(purpose []byte, key []byte) error {
	k := hkdf.New(sha256.New, v.masterKey, v.format.UniqueID, purpose)
	_, err := io.ReadFull(k, key)
	return err
}

func (v *Vault) repoConfig() (*repositoryConfig, error) {
	var rc repositoryConfig

	b, err := v.readEncryptedBlock(repositoryConfigBlock)
	if err != nil {
		return nil, fmt.Errorf("unable to read repository: %v", err)
	}

	err = json.Unmarshal(b, &rc)
	if err != nil {
		return nil, err
	}

	return &rc, nil
}

// RepositoryFormat returns the format of the repository.
func (v *Vault) RepositoryFormat() (*repo.Format, error) {
	rc, err := v.repoConfig()
	if err != nil {
		return nil, err
	}

	return rc.Format, nil
}

// OpenRepository connects to the repository the vault is associated with.
func (v *Vault) OpenRepository() (repo.Repository, error) {
	rc, err := v.repoConfig()
	if err != nil {
		return nil, err
	}

	storage, err := blob.NewStorage(rc.Connection)
	if err != nil {
		return nil, fmt.Errorf("unable to open repository: %v", err)
	}

	return repo.NewRepository(storage, rc.Format)
}

// Get returns the contents of a specified vault item.
func (v *Vault) Get(itemID string) ([]byte, error) {
	return v.readEncryptedBlock(itemID)
}

func (v *Vault) getJSON(itemID string, content interface{}) error {
	j, err := v.readEncryptedBlock(itemID)
	if err != nil {
		return nil
	}

	return json.Unmarshal(j, content)
}

// Put stores the contents of an item stored in a vault with a given ID.
func (v *Vault) putJSON(id string, content interface{}) error {
	j, err := json.Marshal(content)
	if err != nil {
		return err
	}
	return v.Put(id, j)
}

// List returns the list of vault items matching the specified prefix.
func (v *Vault) List(prefix string) ([]string, error) {
	var result []string

	for b := range v.storage.ListBlocks(prefix) {
		if b.Error != nil {
			return result, b.Error
		}
		result = append(result, b.BlockID)
	}
	return result, nil
}

type vaultConfig struct {
	ConnectionInfo blob.ConnectionInfo `json:"connection"`
	Key            []byte              `json:"key,omitempty"`
}

// Token returns a persistent opaque string that encodes the configuration of vault storage
// and its credentials in a way that can be later used to open the vault.
func (v *Vault) Token() (string, error) {
	cip, ok := v.storage.(blob.ConnectionInfoProvider)
	if !ok {
		return "", errors.New("repository does not support persisting configuration")
	}

	vc := vaultConfig{
		ConnectionInfo: cip.ConnectionInfo(),
		Key:            v.masterKey,
	}

	b, err := json.Marshal(&vc)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Remove deletes the specified vault item.
func (v *Vault) Remove(itemID string) error {
	switch itemID {
	case formatBlockID, repositoryConfigBlock, checksumBlockID:
		return fmt.Errorf("item cannot be deleted: %v", itemID)
	default:
		return v.storage.DeleteBlock(itemID)
	}
}

// Create initializes a Vault attached to the specified repository.
func Create(
	vaultStorage blob.Storage,
	vaultFormat *Format,
	vaultCreds Credentials,
	repoStorage blob.Storage,
	repoFormat *repo.Format,
) (*Vault, error) {
	cip, ok := repoStorage.(blob.ConnectionInfoProvider)
	if !ok {
		return nil, errors.New("repository does not support persisting configuration")
	}

	v := Vault{
		storage: vaultStorage,
		format:  *vaultFormat,
	}
	v.format.Version = "1"
	v.format.UniqueID = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, v.format.UniqueID); err != nil {
		return nil, err
	}
	v.masterKey = vaultCreds.getMasterKey(v.format.UniqueID)

	formatBytes, err := json.Marshal(&v.format)
	if err != nil {
		return nil, err
	}

	vaultStorage.PutBlock(
		formatBlockID,
		blob.NewReader(bytes.NewBuffer(formatBytes)),
		blob.PutOptionsOverwrite,
	)

	// Write encrypted checksum block consisting of random bytes with the proper checksum.
	vv := make([]byte, 512)
	if _, err := io.ReadFull(rand.Reader, vv); err != nil {
		return nil, err
	}
	if err := v.Put(checksumBlockID, vv); err != nil {
		return nil, err
	}

	// Write encrypted repository configuration block.
	if err := v.putJSON(repositoryConfigBlock, &repositoryConfig{
		Connection: cip.ConnectionInfo(),
		Format:     repoFormat,
	}); err != nil {
		return nil, err
	}

	return &v, nil
}

// Open opens a vault.
func Open(storage blob.Storage, creds Credentials) (*Vault, error) {
	v := Vault{
		storage: storage,
	}

	f, err := storage.GetBlock(formatBlockID)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(f, &v.format)
	if err != nil {
		return nil, err
	}

	v.masterKey = creds.getMasterKey(v.format.UniqueID)

	if _, err := v.readEncryptedBlock(checksumBlockID); err != nil {
		return nil, err
	}

	return &v, nil
}

// OpenWithToken opens a vault with storage configuration and credentials in the specified token.
func OpenWithToken(token string) (*Vault, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("invalid vault base64 token: %v", err)
	}

	var vc vaultConfig
	err = json.Unmarshal(b, &vc)
	if err != nil {
		return nil, fmt.Errorf("invalid vault json token: %v", err)
	}

	st, err := blob.NewStorage(vc.ConnectionInfo)
	if err != nil {
		return nil, fmt.Errorf("cannot open vault storage: %v", err)
	}

	creds, err := MasterKey(vc.Key)
	if err != nil {
		return nil, fmt.Errorf("invalid vault token")
	}

	return Open(st, creds)
}
