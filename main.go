// terraform-credentials-fastship は、Private Registry (既定は registry.fastship.jp) の Terraform の credentials helper。
// Terraform がモジュールを取得するときに、GitHub Actions の ID トークンを、その場で取って渡す。
// トークンは保管しない (取るたびに新しくなるため、有効期限を気にしなくてよい)。
// PC では使わない (terraform login を使う)。
//
// 使い方 (Terraform が呼ぶ): terraform-credentials-fastship [--host=<registry のホスト>] get <ホスト>
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
	"regexp"
	"strings"
	"time"
)

const (
	// --host を指定しないときのホスト (v2.1.0 までの使い方のまま動かすため)
	defaultHost = "registry.fastship.jp"

	hostFlag = "--host="
)

// hostPattern は、registry のホスト名 (スキーム・ポート・パスを書かない、ドットを含む名前)
var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// audienceOf は、registry が受け付ける audience。v1 の Action (TF_TOKEN に渡す形) と区別するため、/v2 を付ける
func audienceOf(host string) string {
	return "https://" + host + "/v2"
}

// deps は、外の世界とのやり取り。テストで差し替える
type deps struct {
	getenv func(string) string
	client *http.Client
	// wait はリトライの前に待つ時間 (attempt は 1 から)
	wait func(attempt int) time.Duration
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, defaultDeps()))
}

func defaultDeps() deps {
	return deps{
		getenv: os.Getenv,
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
		wait: func(attempt int) time.Duration { return time.Duration(1<<(attempt-1)) * time.Second },
	}
}

// run は helper の本体。終了コードを返す
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, d deps) int {
	registryHost := defaultHost
	var positional []string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, hostFlag):
			registryHost = strings.ToLower(strings.TrimPrefix(arg, hostFlag))
			if !hostPattern.MatchString(registryHost) {
				fmt.Fprintf(stderr, "%s には、registry のホスト名 (registry.example.io の形。スキーム・ポート・パスは書かない) を書いてください: %q\n", hostFlag, registryHost)
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
		fmt.Fprintf(stderr, "使い方: terraform-credentials-fastship [%s<registry のホスト>] get <ホスト>\n", hostFlag)
		return 1
	}
	verb, host := positional[0], positional[1]

	switch verb {
	case "get":
		// 指定したホストのほか (registry.terraform.io など) には、トークンを渡さない
		if !strings.EqualFold(host, registryHost) {
			fmt.Fprintln(stdout, "{}")
			return 0
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		token, err := githubToken(ctx, audienceOf(registryHost), d)
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
		fmt.Fprintln(stderr, "terraform login には対応していません。PC では helper と fastship.tfrc を消してから terraform login してください")
		return 1
	case "forget":
		// 何も保管していないので、消すものも無い
		return 0
	default:
		fmt.Fprintf(stderr, "知らない操作です: %s\n", verb)
		return 1
	}
}

func githubToken(ctx context.Context, audience string, d deps) (string, error) {
	if d.getenv("GITHUB_ACTIONS") != "true" {
		return "", errors.New("GitHub Actions の外では使えません。PC では helper と fastship.tfrc を消してから terraform login してください")
	}
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
		// 名前を引けない・繋がらないは、やり直しても変わらない
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
