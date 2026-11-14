package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ID トークンの代わり。JWT の形 (ドットで区切った 3 つ) だけを真似る (本物の JWT の形にすると、シークレットの検査に掛かる)
const jwt = "header.payload.signature"

// fake は、テストの環境 (環境変数)
type fake struct {
	env map[string]string
}

func (f *fake) deps() deps {
	return deps{
		getenv: func(key string) string { return f.env[key] },
		client: http.DefaultClient,
		wait:   func(int) time.Duration { return 0 },
	}
}

type result struct {
	code   int
	stdout string
	stderr string
}

func runHelper(t *testing.T, f *fake, stdin string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr, f.deps())
	return result{code, stdout.String(), stderr.String()}
}

func tokenOf(t *testing.T, r result) string {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("終了コードが %d (stderr: %s)", r.code, r.stderr)
	}
	var credentials map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &credentials); err != nil {
		t.Fatalf("stdout が JSON ではない: %q", r.stdout)
	}
	return credentials["token"]
}

func Testほかのホストにはトークンを渡さない(t *testing.T) {
	f := &fake{env: map[string]string{"GITHUB_ACTIONS": "true"}}
	r := runHelper(t, f, "", "get", "registry.terraform.io")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "{}" {
		t.Fatalf("got %+v", r)
	}
}

func TestGitHubActionsのIDトークンを返す(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "bearer request-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if req.URL.Query().Get("audience") != "https://registry.fastship.jp/v2" || req.URL.Query().Get("api-version") != "2.0" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"count":1,"value":"`+jwt+`"}`)
	}))
	defer server.Close()
	f := &fake{env: map[string]string{
		"GITHUB_ACTIONS":                 "true",
		"ACTIONS_ID_TOKEN_REQUEST_URL":   server.URL + "/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
	}}
	// Terraform はホスト名を小文字にして渡すが、大文字が混じっても同じホストとして扱う
	if got := tokenOf(t, runHelper(t, f, "", "get", "Registry.Fastship.JP")); got != jwt {
		t.Fatalf("token = %q", got)
	}
}

// githubServer は、GitHub Actions の ID トークンの発行元の代わり。受け取った audience を記録する
func githubServer(t *testing.T, audiences *[]string) *fake {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		*audiences = append(*audiences, req.URL.Query().Get("audience"))
		_, _ = io.WriteString(w, `{"value":"`+jwt+`"}`)
	}))
	t.Cleanup(server.Close)
	return &fake{env: map[string]string{
		"GITHUB_ACTIONS":                 "true",
		"ACTIONS_ID_TOKEN_REQUEST_URL":   server.URL + "/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
	}}
}

func Test指定したホストにはそのホストのv2のaudienceのトークンを返す(t *testing.T) {
	var audiences []string
	f := githubServer(t, &audiences)
	if got := tokenOf(t, runHelper(t, f, "", "--host=registry.example.io", "get", "registry.example.io")); got != jwt {
		t.Fatalf("token = %q", got)
	}
	if len(audiences) != 1 || audiences[0] != "https://registry.example.io/v2" {
		t.Fatalf("audience = %q", audiences)
	}
}

func Testホストの大文字は小文字にそろえる(t *testing.T) {
	var audiences []string
	f := githubServer(t, &audiences)
	if got := tokenOf(t, runHelper(t, f, "", "--host=Registry.Example.IO", "get", "registry.example.io")); got != jwt {
		t.Fatalf("token = %q", got)
	}
	if len(audiences) != 1 || audiences[0] != "https://registry.example.io/v2" {
		t.Fatalf("audience = %q", audiences)
	}
}

func Testホストを指定するとほかのホストにはトークンを渡さない(t *testing.T) {
	var audiences []string
	f := githubServer(t, &audiences)
	// 既定のホスト (registry.fastship.jp) にも渡さない
	for _, host := range []string{defaultHost, "registry.terraform.io"} {
		r := runHelper(t, f, "", "--host=registry.example.io", "get", host)
		if r.code != 0 || strings.TrimSpace(r.stdout) != "{}" {
			t.Fatalf("host = %s: got %+v", host, r)
		}
	}
	if len(audiences) != 0 {
		t.Fatalf("ID トークンを取りに行った: %q", audiences)
	}
}

func TestGitHubActionsの外ではterraform_loginを案内する(t *testing.T) {
	// PC・Cloud Build などでは、メタデータサーバや gcloud に頼らず、すぐに失敗する
	r := runHelper(t, &fake{env: map[string]string{}}, "", "get", defaultHost)
	if r.code != 1 || !strings.Contains(r.stderr, "terraform login") {
		t.Fatalf("got %+v", r)
	}
}

func TestGitHubの一時的な失敗はやり直す(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"value":"`+jwt+`"}`)
	}))
	defer server.Close()
	f := &fake{env: map[string]string{
		"GITHUB_ACTIONS":                 "true",
		"ACTIONS_ID_TOKEN_REQUEST_URL":   server.URL + "/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
	}}
	if got := tokenOf(t, runHelper(t, f, "", "get", defaultHost)); got != jwt || calls != 3 {
		t.Fatalf("token = %q, calls = %d", got, calls)
	}
}

func TestGitHubが拒否したらやり直さずに失敗する(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "secret-body")
	}))
	defer server.Close()
	f := &fake{env: map[string]string{
		"GITHUB_ACTIONS":                 "true",
		"ACTIONS_ID_TOKEN_REQUEST_URL":   server.URL + "/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
	}}
	r := runHelper(t, f, "", "get", defaultHost)
	if r.code != 1 || calls != 1 || !strings.Contains(r.stderr, "403") || strings.Contains(r.stderr, "secret-body") {
		t.Fatalf("got %+v, calls = %d", r, calls)
	}
}

func TestGitHubActionsでidtokenの権限が無いと案内する(t *testing.T) {
	f := &fake{env: map[string]string{"GITHUB_ACTIONS": "true"}}
	r := runHelper(t, f, "", "get", defaultHost)
	if r.code != 1 || !strings.Contains(r.stderr, "id-token: write") {
		t.Fatalf("got %+v", r)
	}
}

func TestGitHubの応答にトークンが無いと失敗する(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"count":0}`)
	}))
	defer server.Close()
	f := &fake{env: map[string]string{
		"GITHUB_ACTIONS":                 "true",
		"ACTIONS_ID_TOKEN_REQUEST_URL":   server.URL + "/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
	}}
	r := runHelper(t, f, "", "get", defaultHost)
	if r.code != 1 || !strings.Contains(r.stderr, "ID トークンの形ではない") {
		t.Fatalf("got %+v", r)
	}
}

func TestStoreは対応しない(t *testing.T) {
	stdin := strings.NewReader(`{"token":"x"}`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"store", defaultHost}, stdin, &stdout, &stderr, (&fake{}).deps())
	if code != 1 || stdin.Len() != 0 || !strings.Contains(stderr.String(), "terraform login") {
		t.Fatalf("code = %d, 残り = %d, stderr = %s", code, stdin.Len(), stderr.String())
	}
}

func TestForgetは何もしない(t *testing.T) {
	r := runHelper(t, &fake{}, "", "forget", defaultHost)
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("got %+v", r)
	}
}

func Test引数が違うと失敗する(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"get"},
		{"list", defaultHost},
		{"--unknown", "get", defaultHost},
		{"--host=", "get", defaultHost},
		{"--host=https://registry.example.io", "get", "registry.example.io"},
		{"--host=registry.example.io/v2", "get", "registry.example.io"},
		{"--host=registry.example.io:443", "get", "registry.example.io"},
		{"--host=registry", "get", "registry"},
		// Google の SA のなりすましは無くした (PC では terraform login を使う)
		{"--impersonate-service-account=tf@acme.iam.gserviceaccount.com", "get", defaultHost},
	} {
		if r := runHelper(t, &fake{}, "", args...); r.code != 1 || r.stderr == "" {
			t.Errorf("args = %q: got %+v", args, r)
		}
	}
}
