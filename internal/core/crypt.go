package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Keyring holds the installation master key. It encrypts stored credentials and
// wraps the per-backup data keys. The key lives in its own file, never in the catalog.
type Keyring struct{ master []byte }

// LoadOrCreateMaster reads the 32-byte master key (hex) or creates it with mode 0600.
func LoadOrCreateMaster(path string) (*Keyring, bool, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != 32 {
			return nil, false, fmt.Errorf("master key file %s is damaged", path)
		}
		return &Keyring{k}, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, false, err
	}
	return &Keyring{k}, true, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Seal encrypts a small secret: nonce || ciphertext.
func (k *Keyring) Seal(plain []byte) ([]byte, error) {
	a, err := gcm(k.master)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, plain, nil), nil
}

func (k *Keyring) Open(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	a, err := gcm(k.master)
	if err != nil {
		return nil, err
	}
	if len(b) < a.NonceSize() {
		return nil, errors.New("sealed value too short")
	}
	return a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], nil)
}

// NewDataKey returns a random key for one backup and its wrapped form.
func (k *Keyring) NewDataKey() (key, wrapped []byte, err error) {
	key = make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return
	}
	wrapped, err = k.Seal(key)
	return
}

// Stream format: "BKM1" | 16-byte salt | frames. Frame: uint32 length | flag (1 = last) | AES-256-GCM
// ciphertext. Each object gets its own key (HKDF of the backup key and the salt), the nonce is the
// frame counter, and the flag is authenticated, so a cut or reordered file is detected.
const chunkSize = 1 << 20

var streamMagic = []byte("BKM1")

type encWriter struct {
	w    io.Writer
	a    cipher.AEAD
	buf  []byte
	n    uint64
	done bool
}

func NewEncWriter(w io.Writer, key []byte) (io.WriteCloser, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	a, err := objectCipher(key, salt)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(append(append([]byte{}, streamMagic...), salt...)); err != nil {
		return nil, err
	}
	return &encWriter{w: w, a: a, buf: make([]byte, 0, chunkSize)}, nil
}

func objectCipher(key, salt []byte) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, key, salt, "backup-manager object", 32)
	if err != nil {
		return nil, err
	}
	return gcm(k)
}

func (e *encWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		room := chunkSize - len(e.buf)
		if room > len(p) {
			room = len(p)
		}
		e.buf = append(e.buf, p[:room]...)
		p = p[room:]
		if len(e.buf) == chunkSize {
			if err := e.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func (e *encWriter) flush(last bool) error {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], e.n)
	e.n++
	flag := []byte{0}
	if last {
		flag[0] = 1
	}
	ct := e.a.Seal(nil, nonce, e.buf, flag)
	head := make([]byte, 5)
	binary.BigEndian.PutUint32(head, uint32(len(ct)))
	head[4] = flag[0]
	if _, err := e.w.Write(head); err != nil {
		return err
	}
	_, err := e.w.Write(ct)
	e.buf = e.buf[:0]
	return err
}

func (e *encWriter) Close() error {
	if e.done {
		return nil
	}
	e.done = true
	return e.flush(true)
}

type decReader struct {
	r    io.Reader
	a    cipher.AEAD
	buf  []byte
	n    uint64
	last bool
}

func NewDecReader(r io.Reader, key []byte) (io.Reader, error) {
	head := make([]byte, 20)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("encrypted stream header: %w", err)
	}
	if string(head[:4]) != string(streamMagic) {
		return nil, errors.New("not an encrypted backup object")
	}
	a, err := objectCipher(key, head[4:])
	if err != nil {
		return nil, err
	}
	return &decReader{r: r, a: a}, nil
}

func (d *decReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.last {
			return 0, io.EOF
		}
		head := make([]byte, 5)
		if _, err := io.ReadFull(d.r, head); err != nil {
			return 0, fmt.Errorf("backup object is cut short: %w", io.ErrUnexpectedEOF)
		}
		size := binary.BigEndian.Uint32(head)
		if size > chunkSize+64 {
			return 0, errors.New("backup object is damaged (frame size)")
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(d.r, ct); err != nil {
			return 0, fmt.Errorf("backup object is cut short: %w", io.ErrUnexpectedEOF)
		}
		nonce := make([]byte, 12)
		binary.BigEndian.PutUint64(nonce[4:], d.n)
		d.n++
		plain, err := d.a.Open(nil, nonce, ct, head[4:5])
		if err != nil {
			return 0, errors.New("backup object failed its integrity check (wrong key or damaged data)")
		}
		d.buf = plain
		d.last = head[4] == 1
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }

// SealHex seals a secret and returns hex, for the settings table.
func (k *Keyring) SealHex(plain string) (string, error) {
	b, err := k.Seal([]byte(plain))
	return hex.EncodeToString(b), err
}

func randRead(b []byte) (int, error) { return rand.Read(b) }
