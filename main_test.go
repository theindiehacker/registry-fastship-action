package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ID トークンの代わり。JWT の形 (ドットで区切った 3 つ) だけを真似る (本物の JWT の形にすると、シークレットの検査に掛かる)
const jwt = "header.payload.signature"

// fake は、テストの環境 (環境変数・gcloud)
type fake struct {
	env        map[string]string
	gcloudArgs []string
	gcloudOut  string
	gcloudErr  error
}

func (f *fake) deps() deps {
	return deps{
		getenv: func(key string) string { return f.env[key] },
		gcloud: func(_ context.Context, args []string, stderr io.Writer) ([]byte, error) {
			f.gcloudArgs = args
			if f.gcloudErr != nil {
				_, _ = io.WriteString(stderr, "ERROR: (gcloud) 失敗\n")
				return nil, f.gcloudErr
			}
			return []byte(f.gcloudOut), nil
		},
		client:         http.DefaultClient,
		metadataClient: http.DefaultClient,
		wait:           func(int) time.Duration { return 0 },
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

// unusedHost は、繋がらないアドレス (メタデータサーバの無い環境)
func unusedHost(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
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
		if req.URL.Query().Get("audience") != audience || req.URL.Query().Get("api-version") != "2.0" {
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
	if got := tokenOf(t, runHelper(t, f, "", "get", registryHost)); got != jwt || calls != 3 {
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
	r := runHelper(t, f, "", "get", registryHost)
	if r.code != 1 || calls != 1 || !strings.Contains(r.stderr, "403") || strings.Contains(r.stderr, "secret-body") {
		t.Fatalf("got %+v, calls = %d", r, calls)
	}
}

func TestGitHubActionsでidtokenの権限が無いと案内する(t *testing.T) {
	// self-hosted のランナーが Google Cloud にあっても、メタデータサーバ (ランナーの SA) には落ちない
	f := &fake{env: map[string]string{"GITHUB_ACTIONS": "true", "GCE_METADATA_HOST": unusedHost(t)}}
	r := runHelper(t, f, "", "get", registryHost)
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
	r := runHelper(t, f, "", "get", registryHost)
	if r.code != 1 || !strings.Contains(r.stderr, "ID トークンの形ではない") {
		t.Fatalf("got %+v", r)
	}
}

func Testメタデータサーバのemail入りのIDトークンを返す(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		if req.Header.Get("Metadata-Flavor") != "Google" ||
			req.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" ||
			q.Get("audience") != audience || q.Get("format") != "full" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, jwt)
	}))
	defer server.Close()
	f := &fake{env: map[string]string{"GCE_METADATA_HOST": strings.TrimPrefix(server.URL, "http://")}}
	if got := tokenOf(t, runHelper(t, f, "", "get", registryHost)); got != jwt {
		t.Fatalf("token = %q", got)
	}
}

func TestどこでもなければIDトークンを取れる環境を案内する(t *testing.T) {
	f := &fake{env: map[string]string{"GCE_METADATA_HOST": unusedHost(t)}}
	r := runHelper(t, f, "", "get", registryHost)
	if r.code != 1 || !strings.Contains(r.stderr, "ID トークンを取れる環境ではありません") {
		t.Fatalf("got %+v", r)
	}
}

func TestSAになりすましてIDトークンを返す(t *testing.T) {
	// GitHub Actions の中でも、CLI 設定で SA を指定したら、なりすましを使う
	f := &fake{env: map[string]string{"GITHUB_ACTIONS": "true"}, gcloudOut: jwt + "\n"}
	got := tokenOf(t, runHelper(t, f, "", "--impersonate-service-account=tf@acme.iam.gserviceaccount.com", "get", registryHost))
	if got != jwt {
		t.Fatalf("token = %q", got)
	}
	want := []string{
		"auth", "print-identity-token",
		"--impersonate-service-account=tf@acme.iam.gserviceaccount.com",
		"--audiences=https://registry.fastship.jp",
		"--include-email",
	}
	if strings.Join(f.gcloudArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("gcloud の引数 = %q", f.gcloudArgs)
	}
}

func TestなりすましできなければgcloudのエラーとともにSAを案内する(t *testing.T) {
	f := &fake{gcloudErr: errors.New("exit status 1")}
	r := runHelper(t, f, "", "--impersonate-service-account=tf@acme.iam.gserviceaccount.com", "get", registryHost)
	if r.code != 1 || !strings.Contains(r.stderr, "ERROR: (gcloud)") || !strings.Contains(r.stderr, "serviceAccountTokenCreator") {
		t.Fatalf("got %+v", r)
	}
}

func TestStoreは対応しない(t *testing.T) {
	stdin := strings.NewReader(`{"token":"x"}`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"store", registryHost}, stdin, &stdout, &stderr, (&fake{}).deps())
	if code != 1 || stdin.Len() != 0 || !strings.Contains(stderr.String(), "terraform login") {
		t.Fatalf("code = %d, 残り = %d, stderr = %s", code, stdin.Len(), stderr.String())
	}
}

func TestForgetは何もしない(t *testing.T) {
	r := runHelper(t, &fake{}, "", "forget", registryHost)
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("got %+v", r)
	}
}

func Test引数が違うと失敗する(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"get"},
		{"list", registryHost},
		{"--unknown", "get", registryHost},
		{"--impersonate-service-account=", "get", registryHost},
	} {
		if r := runHelper(t, &fake{}, "", args...); r.code != 1 || r.stderr == "" {
			t.Errorf("args = %q: got %+v", args, r)
		}
	}
}
