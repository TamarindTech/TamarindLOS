package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// --- Blob storage ------------------------------------------------------------------

// ErrTooLarge is returned when an upload exceeds its size limit.
var ErrTooLarge = errors.New("file is larger than allowed")

// BlobStore keeps uploaded files. The Lite topology uses the local file system;
// production deployments use S3-compatible object storage behind the same port.
type BlobStore interface {
	// Put stores at most maxBytes from r under key and returns the SHA-256 and size.
	Put(ctx context.Context, key string, r io.Reader, maxBytes int64) (sha string, size int64, err error)
	Get(ctx context.Context, key string) ([]byte, error)
}

// FSBlobStore stores files under a directory.
type FSBlobStore struct{ Dir string }

func (s FSBlobStore) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(key, "..") || clean == "/" {
		return "", fmt.Errorf("invalid blob key")
	}
	return filepath.Join(s.Dir, clean), nil
}

// Put implements BlobStore. The file is written to a temporary name and renamed when complete.
func (s FSBlobStore) Put(_ context.Context, key string, r io.Reader, maxBytes int64) (string, int64, error) {
	p, err := s.path(key)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if n > maxBytes {
		return "", 0, ErrTooLarge
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Get implements BlobStore.
func (s FSBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// --- Document processing ports -------------------------------------------------------

// MalwareScanner checks uploaded files.
type MalwareScanner interface {
	Scan(ctx context.Context, content []byte) (clean bool, finding string, err error)
}

// Classification is a document type with a confidence score.
type Classification struct {
	DocumentType string
	Confidence   float64
	Model        string
}

// DocumentClassifier decides what a document is.
type DocumentClassifier interface {
	Classify(ctx context.Context, content []byte, fileName, declaredType string) (Classification, error)
}

// ExtractedField is one value read from a document, with its provenance.
type ExtractedField struct {
	Field      string
	Value      string
	Confidence float64
	Page       int
}

// Extractor reads typed fields from a classified document. It has no tool access:
// document text is data and never becomes instructions.
type Extractor interface {
	Extract(ctx context.Context, content []byte, documentType string) (fields []ExtractedField, extractor, version string, err error)
}

// DocumentTypes known to the mock classifier (and to the baseline pack).
var DocumentTypes = []string{"business_registration", "identity_document", "bank_statements",
	"financial_statements", "management_accounts", "proof_of_address", "tax_return"}

// --- Mock adapters ----------------------------------------------------------------------
//
// Mock documents are small files whose text says what they are and what they contain:
//
//	%PDF-1.4
//	% TAMARIND-DOC: business_registration
//	registration_number: 0105561234567
//	legal_name: Mekong Trading Co.
//
// A value ending in "?" is read with low confidence. Without the marker, the declared
// type and then the file name are used, with lower confidence.

// eicar is the standard antivirus test signature.
const eicar = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

var (
	markerRe = regexp.MustCompile(`TAMARIND-DOC:\s*([a-z_]+)`)
	fieldRe  = regexp.MustCompile(`(?m)^\s*([a-z][a-z0-9_]*)\s*:\s*(.+?)\s*$`)
)

// Scan implements MalwareScanner.
func (m *Mock) Scan(_ context.Context, content []byte) (bool, string, error) {
	if bytes.Contains(content, []byte(eicar)) {
		return false, "EICAR test signature", nil
	}
	return true, "", nil
}

func knownType(t string) bool {
	for _, k := range DocumentTypes {
		if k == t {
			return true
		}
	}
	return false
}

var fileNameHints = []struct{ word, docType string }{
	{"registration", "business_registration"}, {"certificate", "business_registration"},
	{"passport", "identity_document"}, {"identity", "identity_document"}, {"id_card", "identity_document"},
	{"statement", "bank_statements"}, {"bank", "bank_statements"},
	{"financial", "financial_statements"}, {"accounts", "management_accounts"},
	{"utility", "proof_of_address"}, {"address", "proof_of_address"}, {"tax", "tax_return"},
}

// Classify implements DocumentClassifier.
func (m *Mock) Classify(_ context.Context, content []byte, fileName, declaredType string) (Classification, error) {
	c := Classification{DocumentType: "other", Confidence: 0.3, Model: "mock-classifier@1"}
	switch {
	case markerRe.Match(content):
		t := string(markerRe.FindSubmatch(content)[1])
		if knownType(t) {
			c.DocumentType, c.Confidence = t, 0.97
		}
	case knownType(declaredType):
		c.DocumentType, c.Confidence = declaredType, 0.82
	default:
		name := strings.ToLower(fileName)
		for _, h := range fileNameHints {
			if strings.Contains(name, h.word) {
				c.DocumentType, c.Confidence = h.docType, 0.75
				break
			}
		}
	}
	return c, nil
}

// Extract implements Extractor.
func (m *Mock) Extract(_ context.Context, content []byte, documentType string) ([]ExtractedField, string, string, error) {
	var fields []ExtractedField
	for _, match := range fieldRe.FindAllSubmatch(content, -1) {
		name, value := string(match[1]), string(match[2])
		if strings.HasPrefix(strings.TrimSpace(name), "pdf") {
			continue
		}
		conf := 0.95
		if strings.HasSuffix(value, "?") {
			value, conf = strings.TrimSpace(strings.TrimSuffix(value, "?")), 0.6
		}
		fields = append(fields, ExtractedField{Field: name, Value: value, Confidence: conf, Page: 1})
	}
	return fields, "mock-extractor", "1." + documentType, nil
}
