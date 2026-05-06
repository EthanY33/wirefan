package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EthanY33/wirefan/internal/auth"
	"github.com/EthanY33/wirefan/internal/store"
)

func TestCreateAndListKeys(t *testing.T) {
	s := store.NewMemory()
	rest := NewRestHandler(s, "admin-tok", "test-signing-secret")
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// create
	body := bytes.NewBufferString(`{"name":"app1"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/keys", body)
	req.Header.Set("Authorization", "Bearer admin-tok")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("want 201, got %d", res.StatusCode)
	}
	var created struct{ ID, Secret string }
	json.NewDecoder(res.Body).Decode(&created)
	if created.ID == "" || created.Secret == "" {
		t.Fatal("expected id and secret")
	}

	// list
	req2, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
	req2.Header.Set("Authorization", "Bearer admin-tok")
	res2, _ := http.DefaultClient.Do(req2)
	body2, _ := io.ReadAll(res2.Body)
	if !bytes.Contains(body2, []byte(created.ID)) {
		t.Fatalf("list missing id: %s", body2)
	}
}

func TestRequiresAdminBearer(t *testing.T) {
	s := store.NewMemory()
	rest := NewRestHandler(s, "tok", "test-signing-secret")
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/keys", nil)
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", res.StatusCode)
	}
}

func TestAuthSign(t *testing.T) {
	s := store.NewMemory()
	secret, _ := auth.GenerateSecret()
	k, _ := s.CreateKey(context.Background(), "app", auth.HashSecret(secret))
	rest := NewRestHandler(s, "admin-tok", "server-signing-secret")
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := bytes.NewBufferString(`{"socket_id":"01HX","channel":"private-room"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/auth/sign", body)
	req.Header.Set("Authorization", "Bearer "+k.ID+":"+secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", res.StatusCode)
	}
	var got struct{ Token string }
	json.NewDecoder(res.Body).Decode(&got)
	if err := auth.VerifyToken("server-signing-secret", "01HX", "private-room", got.Token); err != nil {
		t.Fatal(err)
	}
}
