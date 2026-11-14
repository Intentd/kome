package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
)

// EncodeRecord returns rec as the value of the record annotation key on an
// object whose annotations will be ann (key's own value aside). The record is
// JSON when it fits within the API server's limit on an object's annotations,
// and gzip-compressed JSON in base64 when only that fits. Past the limit even
// compressed, the write would be refused, so it is an error: a migration
// checks before writing, in a dry run too.
func EncodeRecord(rec any, key string, ann map[string]string) (string, error) {
	plain, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("encode the record: %w", err)
	}
	all := make(map[string]string, len(ann)+1)
	for k, v := range ann {
		all[k] = v
	}
	all[key] = string(plain)
	if apivalidation.ValidateAnnotationsSize(all) == nil {
		return string(plain), nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return "", fmt.Errorf("compress the record: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("compress the record: %w", err)
	}
	all[key] = base64.StdEncoding.EncodeToString(buf.Bytes())
	if apivalidation.ValidateAnnotationsSize(all) != nil {
		return "", fmt.Errorf("the record is too large, even compressed: the annotations would be %d bytes, Kubernetes allows %d",
			annotationBytes(all), apivalidation.TotalAnnotationSizeLimitB)
	}
	return all[key], nil
}

// DecodeRecord decodes a record annotation's value into v: JSON, or
// gzip-compressed JSON in base64 (see EncodeRecord). JSON starts with '{';
// the base64 form never does.
func DecodeRecord(raw string, v any) error {
	data := []byte(raw)
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		zipped, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return fmt.Errorf("decode the record: %w", err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(zipped))
		if err != nil {
			return fmt.Errorf("decompress the record: %w", err)
		}
		if data, err = io.ReadAll(zr); err != nil {
			return fmt.Errorf("decompress the record: %w", err)
		}
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode the record: %w", err)
	}
	return nil
}

func annotationBytes(ann map[string]string) int {
	n := 0
	for k, v := range ann {
		n += len(k) + len(v)
	}
	return n
}
