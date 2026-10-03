// Package githubtest is a GitHub that holds one file of one repository and knows one App. It
// checks what the real one checks: the App's signed assertion, the installation token, and the
// blob a write claims to replace.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // a blob id, as git computes one; not a security boundary.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	AppID          = "5165602"
	InstallationID = "167270563"
	Repository     = "JorisJonkers-dev/estate"
	Path           = "cluster-state.yml"
	Branch         = "main"
	token          = "installation-token"
)

// Commit is one write the server accepted.
type Commit struct {
	Message  string
	Document string
}

// Server is the fake. Document and Commits are read after the code under test has run.
type Server struct {
	*httptest.Server
	Key *rsa.PrivateKey

	mu       sync.Mutex
	document []byte
	commits  []Commit
}

// New starts a GitHub holding document at Path, and generates the App's key.
func New(t *testing.T, document string) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Key: key, document: []byte(document)}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/"+InstallationID+"/access_tokens", s.grant)
	mux.HandleFunc("GET /repos/"+Repository+"/contents/"+Path, s.read)
	mux.HandleFunc("PUT /repos/"+Repository+"/contents/"+Path, s.write)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// PEM is the App's private key as GitHub hands it out.
func (s *Server) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.Key)})
}

// Document is the file as it stands.
func (s *Server) Document() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.document)
}

// Set replaces the file behind the code under test's back, as another writer would.
func (s *Server) Set(document string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.document = []byte(document)
}

// Commits are the writes accepted so far.
func (s *Server) Commits() []Commit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Commit(nil), s.commits...)
}

func (s *Server) grant(w http.ResponseWriter, r *http.Request) {
	if !s.signedByTheApp(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
		http.Error(w, "a JSON web token could not be decoded", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// signedByTheApp verifies the assertion the way GitHub does: RS256, by this App, not expired,
// and good for no more than ten minutes.
func (s *Server) signedByTheApp(assertion string) bool {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&s.Key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Issuer   string `json:"iss"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	now := time.Now().Unix()
	return claims.Issuer == AppID && claims.IssuedAt <= now && claims.Expires > now && claims.Expires-claims.IssuedAt <= 600
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.URL.Query().Get("ref") != Branch {
		http.Error(w, "no such ref", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// GitHub wraps the base64 it answers with at 60 columns.
	encoded := base64.StdEncoding.EncodeToString(s.document)
	var wrapped strings.Builder
	for len(encoded) > 60 {
		wrapped.WriteString(encoded[:60] + "\n")
		encoded = encoded[60:]
	}
	wrapped.WriteString(encoded + "\n")
	_ = json.NewEncoder(w).Encode(map[string]string{"content": wrapped.String(), "encoding": "base64", "sha": blob(s.document)})
}

func (s *Server) write(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Branch != Branch {
		http.Error(w, "unprocessable", http.StatusUnprocessableEntity)
		return
	}
	document, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		http.Error(w, "unprocessable", http.StatusUnprocessableEntity)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if body.SHA != blob(s.document) {
		http.Error(w, "the file changed since it was read", http.StatusConflict)
		return
	}
	s.document = document
	s.commits = append(s.commits, Commit{Message: body.Message, Document: string(document)})
	_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]string{"sha": blob(document)}})
}

func blob(document []byte) string {
	sum := sha1.Sum(document) //nolint:gosec // see the import.
	return hex.EncodeToString(sum[:])
}
