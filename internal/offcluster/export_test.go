package offcluster

import (
	"context"
	"io"
	"net/http"
)

// Fetch reads one object back whole, as a restore would before it decrypts.
func Fetch(ctx context.Context, s Store, key string) ([]byte, error) {
	req, err := s.request(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	res, err := s.send(req, emptyHash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	return io.ReadAll(res.Body)
}
