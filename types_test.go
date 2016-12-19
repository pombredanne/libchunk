package libchunk_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/advanderveer/libchunk"
	"github.com/boltdb/bolt"
	"github.com/kr/s3"
	"github.com/restic/chunker"
)

type httpRemote struct {
	scheme string
	host   string
	client *http.Client
}

func (r *httpRemote) Get(k libchunk.K) (chunk []byte, err error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s://%s/%s", r.scheme, r.host, k), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request for '%s': %v", k, err)
	}

	s3.Sign(req, s3.Keys{AccessKey: "access-key-id", SecretKey: "secret-key-id"})
	resp, err := r.client.Do(req)
	if err != nil || resp == nil || resp.StatusCode != 200 {
		if resp.StatusCode == 404 {
			return nil, os.ErrNotExist
		}

		return nil, fmt.Errorf("failed to perform HTTP request for '%s': %v", k, err)
	}

	defer resp.Body.Close()
	chunk, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read HTTP response body: %v", err)
	}

	return chunk, nil
}

func (r *httpRemote) Put(k libchunk.K, chunk []byte) (err error) {
	req, err := http.NewRequest("POST", fmt.Sprintf("%s://%s/%s", r.scheme, r.host, k), bytes.NewReader(chunk))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request for '%s': %v", k, err)
	}

	s3.Sign(req, s3.Keys{AccessKey: "access-key-id", SecretKey: "secret-key-id"})
	resp, err := r.client.Do(req)
	if err != nil || resp == nil || resp.StatusCode != 200 {
		return fmt.Errorf("failed to perform HTTP request for '%x': %v", k, err)
	}

	return nil
}

type boltStore struct {
	db         *bolt.DB
	bucketName []byte
}

func (s *boltStore) Put(k libchunk.K, chunk []byte) (err error) {
	return s.db.Batch(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(s.bucketName)
		if err != nil {
			return fmt.Errorf("failed to create-if-not-exist: %v", err)
		}

		existing := b.Get(k[:])
		if existing != nil {
			return nil
		}

		return b.Put(k[:], chunk)
	})
}

func (s *boltStore) Get(k libchunk.K) (chunk []byte, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucketName)
		if b == nil {
			return fmt.Errorf("bucket '%s' must first be created", string(s.bucketName))
		}

		v := b.Get(k[:])
		if v == nil || len(v) < 1 {
			return os.ErrNotExist
		}

		chunk = make([]byte, len(v))
		copy(chunk, v)
		return nil
	})

	return chunk, err
}

type quiter interface {
	Fatalf(format string, args ...interface{})
}

var secret = libchunk.Secret{
	0x3D, 0xA3, 0x35, 0x8B, 0x4D, 0xC1, 0x73, 0x00, //polynomial
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, //random bytes
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

func randr(size int64) io.Reader {
	return io.LimitReader(rand.New(rand.NewSource(time.Now().UnixNano())), size)
}

func randb(size int64) []byte {
	b, err := ioutil.ReadAll(randr(size))
	if err != nil {
		panic(err)
	}

	return b
}

type failingWriter struct {
	*bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (n int, err error) {
	return 0, fmt.Errorf("writer_failure")
}

type failingKeyIterator struct {
	*sliceKeyIterator
}

func (iter *failingKeyIterator) Next() (k libchunk.K, err error) {
	return k, fmt.Errorf("iterator_failure")
}

type httpServer struct {
	chunks map[libchunk.K][]byte
}

func (srv *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		io.Copy(ioutil.Discard, r.Body)
	} else {
		k, err := libchunk.DecodeKey(bytes.TrimLeft([]byte(r.URL.String()), "/"))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		chunk, ok := srv.chunks[k]
		if !ok {
			w.WriteHeader(404)
			return
		}

		_, err = io.Copy(w, bytes.NewReader(chunk))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
}

type failingStore struct{}

func (store *failingStore) Put(k libchunk.K, c []byte) error {
	return fmt.Errorf("storage_failed")
}

func (store *failingStore) Get(k libchunk.K) (c []byte, err error) {
	return c, fmt.Errorf("storage_failed")
}

type emptyStore struct{}

func (store *emptyStore) Put(k libchunk.K, c []byte) error {
	return nil
}

func (store *emptyStore) Get(k libchunk.K) (c []byte, err error) {
	return c, os.ErrNotExist
}

func defaultConfigWithRemote(t quiter, remoteChunks map[libchunk.K][]byte) libchunk.Config {
	conf := defaultConfig(t)
	l, err := net.Listen("tcp", ":")
	if err != nil {
		t.Fatalf("failed to setup test server: %v", err)
	}

	go func() {
		t.Fatalf("failed to serve: %v", http.Serve(l, &httpServer{remoteChunks}))
	}()

	conf.Remote = &httpRemote{"http", l.Addr().String(), &http.Client{}}
	return conf
}

func defaultConfig(t quiter) libchunk.Config {
	dbdir, err := ioutil.TempDir("", "libchunk_db_")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbpath := filepath.Join(dbdir, "db.bolt")
	db, err := bolt.Open(dbpath, 0777, nil)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("chunks"))
		return err
	})

	block, err := aes.NewCipher(secret[:])
	if err != nil {
		t.Fatalf("failed to create AES block cipher: %v", err)
	}

	aead, err := cipher.NewGCMWithNonceSize(block, sha256.Size)
	if err != nil {
		t.Fatalf("failed to setup GCM cipher mode: %v", err)
	}

	conf := libchunk.Config{
		Secret:           secret,
		SplitBufSize:     chunker.MaxSize,
		SplitConcurrency: 64,
		AEAD:             aead,
		KeyHash: func(b []byte) libchunk.K {
			return sha256.Sum256(b)
		},
		Store:           &boltStore{db, []byte("chunks")},
		PushConcurrency: 64,
	}

	return conf
}

func forceRemoteConfig(t *testing.T) libchunk.Config {
	conf := defaultConfigWithRemote(t, nil)
	conf.Store = &emptyStore{}
	return conf
}

func failingStorageConfig(t *testing.T) libchunk.Config {
	block, err := aes.NewCipher(secret[:])
	if err != nil {
		t.Fatalf("failed to create AES block cipher: %v", err)
	}

	aead, err := cipher.NewGCMWithNonceSize(block, sha256.Size)
	if err != nil {
		t.Fatalf("failed to setup GCM cipher mode: %v", err)
	}

	return libchunk.Config{
		Secret:           secret,
		SplitBufSize:     chunker.MaxSize,
		SplitConcurrency: 64,
		AEAD:             aead,
		KeyHash: func(b []byte) libchunk.K {
			return sha256.Sum256(b)
		},
		Store: &failingStore{},
	}
}

type randomBytesInput struct {
	io.Reader
}

func (input *randomBytesInput) Chunker(conf libchunk.Config) (libchunk.Chunker, error) {
	return chunker.New(input, conf.Secret.Pol()), nil
}

type failingChunkerInput struct {
	*bytes.Buffer
}

func (input *failingChunkerInput) Next([]byte) (c chunker.Chunk, err error) {
	return c, fmt.Errorf("chunking_failed")
}

func (input *failingChunkerInput) Chunker(conf libchunk.Config) (libchunk.Chunker, error) {
	return input, nil
}

type failingInput struct {
	*bytes.Buffer
}

func (input *failingInput) Next([]byte) (c chunker.Chunk, err error) {
	return c, nil
}

func (input *failingInput) Chunker(conf libchunk.Config) (libchunk.Chunker, error) {
	return input, fmt.Errorf("input_failed")
}