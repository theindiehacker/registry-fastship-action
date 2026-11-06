// terraform-credentials-fastship は、registry.fastship.jp の Terraform の credentials helper。
// Terraform がモジュールを取得するときに、動いている環境の ID (GitHub Actions・Google Cloud の SA) の ID トークンを、その場で取って渡す。
// トークンは保管しない (取るたびに新しくなるため、有効期限を気にしなくてよい)。
//
// 使い方 (Terraform が呼ぶ): terraform-credentials-fastship [--impersonate-service-account=<SA>] get <ホスト>
// プロトコル: https://developer.hashicorp.com/terraform/internals/credentials-helpers
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	registryHost = "registry.fastship.jp"
	audience     = "https://registry.fastship.jp"

	impersonateFlag = "--impersonate-service-account="
)

// errNoSource は、ID トークンを取れる環境 (GitHub Actions・Google Cloud・SA のなりすまし) でないこと
var errNoSource = errors.New(`ID トークンを取れる環境ではありません。
- ローカル: CLI 設定の credentials_helper "fastship" の args に "` + impersonateFlag + `<登録した SA>" を書き、gcloud auth login でログインしてください
- GitHub Actions: theindiehacker/registry-fastship-action を使い、job に permissions: id-token: write を付けてください
- Google Cloud: メタデータサーバのある環境 (Cloud Build・Compute Engine など) で動かしてください`)

// deps は、外の世界とのやり取り。テストで差し替える
type deps struct {
	getenv func(string) string
	// gcloud は gcloud を args で動かし、stdout を返す。gcloud の stderr は、そのまま helper の stderr に出す
	gcloud func(ctx context.Context, args []string, stderr io.Writer) ([]byte, error)
	// client は GitHub に、metadataClient はメタデータサーバに使う
	client         *http.Client
	metadataClient *http.Client
	// wait はリトライの前に待つ時間 (attempt は 1 から)
	wait func(attempt int) time.Duration
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, defaultDeps()))
}

func defaultDeps() deps {
	return deps{
		getenv: os.Getenv,
		gcloud: func(ctx context.Context, args []string, stderr io.Writer) ([]byte, error) {
			cmd := exec.CommandContext(ctx, "gcloud", args...)
			cmd.Stderr = stderr
			return cmd.Output()
		},
		// 発行元が応答しないときに、job の timeout まで止まらないよう、時間を区切る
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
			Timeout: 30 * time.Second,
		},
		// メタデータサーバはプロキシを通さない (Google のクライアントライブラリと同じ)。
		// Google Cloud の外では、名前が引けないか繋がらずに失敗するので、接続の時間を短くする
		metadataClient: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
				ResponseHeaderTimeout: 30 * time.Second,
			},
			Timeout: 30 * time.Second,
		},
		wait: func(attempt int) time.Duration { return time.Duration(1<<(attempt-1)) * time.Second },
	}
}

// run は helper の本体。終了コードを返す
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, d deps) int {
	var serviceAccount string
	var positional []string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, impersonateFlag):
			serviceAccount = strings.TrimPrefix(arg, impersonateFlag)
			if serviceAccount == "" {
				fmt.Fprintf(stderr, "%s に SA のメールアドレスを書いてください\n", impersonateFlag)
				return 1
			}
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "知らない引数です: %s\n", arg)
			return 1
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) != 2 {
		fmt.Fprintf(stderr, "使い方: terraform-credentials-fastship [%s<SA>] get <ホスト>\n", impersonateFlag)
		return 1
	}
	verb, host := positional[0], positional[1]

	switch verb {
	case "get":
		// registry.fastship.jp のほか (registry.terraform.io など) には、トークンを渡さない
		if !strings.EqualFold(host, registryHost) {
			fmt.Fprintln(stdout, "{}")
			return 0
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		token, err := identityToken(ctx, serviceAccount, d, stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := json.NewEncoder(stdout).Encode(map[string]string{"token": token}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "store":
		// 失敗するときも、Terraform が送ってくる資格情報を読み切る (プロトコルの決まり)
		_, _ = io.Copy(io.Discard, stdin)
		fmt.Fprintln(stderr, "terraform login には対応していません (ID トークンを毎回その場で取るため、保管は要りません)")
		return 1
	case "forget":
		// 何も保管していないので、消すものも無い
		return 0
	default:
		fmt.Fprintf(stderr, "知らない操作です: %s\n", verb)
		return 1
	}
}

// identityToken は、環境に合った方法で ID トークンを取る
func identityToken(ctx context.Context, serviceAccount string, d deps, stderr io.Writer) (string, error) {
	// 1. SA のなりすまし (ローカル)。CLI 設定で指定したときだけ使う
	if serviceAccount != "" {
		return impersonatedToken(ctx, serviceAccount, d, stderr)
	}
	// 2. GitHub Actions。self-hosted のランナーが Google Cloud にあっても、ランナーの SA ではなく、ワークフローの ID を使う
	if d.getenv("GITHUB_ACTIONS") == "true" {
		return githubToken(ctx, d)
	}
	// 3. Google Cloud のメタデータサーバ (Cloud Build・Compute Engine など)
	return metadataToken(ctx, d)
}

func impersonatedToken(ctx context.Context, serviceAccount string, d deps, stderr io.Writer) (string, error) {
	out, err := d.gcloud(ctx, []string{
		"auth", "print-identity-token",
		"--impersonate-service-account=" + serviceAccount,
		"--audiences=" + audience,
		// email が無いと、registry が契約企業を引けず 401 になる
		"--include-email",
	}, stderr)
	if err != nil {
		var notFound *exec.Error
		if errors.As(err, &notFound) {
			return "", errors.New("gcloud が見つかりません。Google Cloud CLI を入れてください")
		}
		return "", fmt.Errorf("gcloud で %s の ID トークンを取れませんでした (gcloud auth login と、SA の roles/iam.serviceAccountTokenCreator を確かめてください): %w", serviceAccount, err)
	}
	return checkToken(string(out))
}

func githubToken(ctx context.Context, d deps) (string, error) {
	requestURL, requestToken := d.getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), d.getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if requestURL == "" || requestToken == "" {
		return "", errors.New("GitHub Actions の ID トークンを取れません。job に permissions: id-token: write を付けてください (fork からの PR では取れません)")
	}
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("ACTIONS_ID_TOKEN_REQUEST_URL が URL ではありません: %w", err)
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()

	body, err := fetch(ctx, d.client, d.wait, u.String(), map[string]string{"Authorization": "bearer " + requestToken})
	if err != nil {
		return "", fmt.Errorf("GitHub から ID トークンを取れませんでした。時間をおいて再実行してください: %w", err)
	}
	var response struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("GitHub の応答を読めませんでした: %w", err)
	}
	return checkToken(response.Value)
}

func metadataToken(ctx context.Context, d deps) (string, error) {
	host := d.getenv("GCE_METADATA_HOST") // Google のクライアントライブラリと同じく、エミュレーターなどに向けられる
	if host == "" {
		host = "metadata.google.internal"
	}
	u := url.URL{
		Scheme:   "http",
		Host:     host,
		Path:     "/computeMetadata/v1/instance/service-accounts/default/identity",
		RawQuery: url.Values{"audience": {audience}, "format": {"full"}}.Encode(), // full: email が入る
	}
	body, err := fetch(ctx, d.metadataClient, d.wait, u.String(), map[string]string{"Metadata-Flavor": "Google"})
	if err != nil {
		if isDialError(err) {
			return "", errNoSource
		}
		return "", fmt.Errorf("メタデータサーバから ID トークンを取れませんでした: %w", err)
	}
	return checkToken(string(body))
}

// fetch は GET し、本文を返す。一時的な失敗 (接続の後の失敗・408・429・5xx) は、間をあけて 3 回までやり直す
func fetch(ctx context.Context, client *http.Client, wait func(int) time.Duration, target string, header map[string]string) ([]byte, error) {
	const retries = 3
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, errors.Join(lastErr, ctx.Err())
			case <-time.After(wait(attempt)):
			}
		}
		body, retryable, err := fetchOnce(ctx, client, target, header)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			break
		}
	}
	return nil, lastErr
}

func fetchOnce(ctx context.Context, client *http.Client, target string, header map[string]string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		// 繋がらない (Google Cloud の外のメタデータサーバなど) は、やり直しても変わらない
		return nil, !isDialError(err), err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, true, err
	}
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		// 本文はトークンを含みうるので出さない
		return nil, retryable, fmt.Errorf("%s が %d を返しました", req.URL.Host, resp.StatusCode)
	}
	return body, false, nil
}

// isDialError は、名前を引けない・繋がらないこと
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// checkToken は、取れた値が JWT の形 (ドットで区切った 3 つ) かを確かめる
func checkToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" || strings.ContainsAny(token, " \t\r\n\"") || strings.Count(token, ".") != 2 {
		return "", errors.New("ID トークンの形ではない値が返りました")
	}
	return token, nil
}
