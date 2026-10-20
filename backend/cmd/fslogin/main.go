// Command fslogin installs login cookies into the FlareSolverr cookie jar and
// verifies that they actually work.
//
// The cookies come from a browser the operator logged into by hand: the
// FlareSolverr service cannot complete an interactive login, and driving a
// local browser over the DevTools protocol would need a dependency the
// project does not have.
//
// Typical use:
//
//	# export the cookies from the browser, then
//	fslogin --host tieba.baidu.com --from cookies.json --jar /etc/pentagi/flaresolverr-cookies.json
//	fslogin --host www.zhihu.com --verify https://www.zhihu.com/search?q=test
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"pentagi/pkg/tools/flaresolverr"

	"github.com/sirupsen/logrus"
)

type cookieEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

func main() {
	host := flag.String("host", "", "host the cookies belong to (e.g. tieba.baidu.com)")
	from := flag.String("from", "", "JSON file with the cookies; use - for stdin")
	jar := flag.String("jar", "", "cookie jar to write")
	verify := flag.String("verify", "", "URL to fetch after installing, to check the login state")
	fsURL := flag.String("flaresolverr", "http://flaresolverr:8191", "FlareSolverr base URL")
	flag.Parse()

	if *host == "" {
		fail("host is required")
	}
	if *jar == "" {
		fail("jar is required")
	}

	cookies, err := readCookies(*from)
	if err != nil {
		fail("failed to read cookies: %v", err)
	}
	if len(cookies) == 0 {
		fail("no cookies found")
	}

	cookieJar, err := flaresolverr.LoadCookieJar(*jar)
	if err != nil {
		fail("failed to load cookie jar: %v", err)
	}
	cookieJar.Set(*host, cookies)
	if err := cookieJar.Save(*jar); err != nil {
		fail("failed to save cookie jar: %v", err)
	}
	logrus.Infof("installed %d cookies for %s into %s", len(cookies), *host, *jar)

	if *verify == "" {
		return
	}
	verifyLogin(*fsURL, *jar, *verify)
}

// readCookies reads cookies from a JSON file or stdin. The file may hold
// either a flat list of cookies or a map of host to cookies, which is the
// shape browser extensions export.
func readCookies(path string) ([]flaresolverr.Cookie, error) {
	var data []byte
	var err error
	if path == "-" || path == "" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty input")
	}

	// A flat list of cookies.
	var list []cookieEntry
	if err := json.Unmarshal(data, &list); err == nil {
		return toCookies(list), nil
	}

	// A map of host to cookies.
	var byHost map[string][]cookieEntry
	if err := json.Unmarshal(data, &byHost); err == nil {
		var out []flaresolverr.Cookie
		for _, entries := range byHost {
			out = append(out, toCookies(entries)...)
		}
		return out, nil
	}

	return nil, fmt.Errorf("unrecognised cookie format")
}

func toCookies(entries []cookieEntry) []flaresolverr.Cookie {
	out := make([]flaresolverr.Cookie, 0, len(entries))
	for _, entry := range entries {
		if entry.Name == "" || entry.Value == "" {
			continue
		}
		path := entry.Path
		if path == "" {
			path = "/"
		}
		out = append(out, flaresolverr.Cookie{
			Name:  entry.Name,
			Value: entry.Value,
			Path:  path,
		})
	}
	return out
}

// verifyLogin fetches the URL through FlareSolverr with the installed cookies
// and reports whether the page still looks like a login wall.
func verifyLogin(fsURL, jarPath, target string) {
	cookieJar, err := flaresolverr.LoadCookieJar(jarPath)
	if err != nil {
		logrus.Errorf("failed to load cookie jar for verification: %v", err)
		return
	}
	client := flaresolverr.NewClient(fsURL, flaresolverr.WithCookieJar(cookieJar))

	// A challenge solve routinely takes up to a minute and the challenge is
	// probabilistic, so the verification needs room for several attempts.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	solution, err := client.FetchWithRetry(ctx, target, 3)
	if err != nil {
		logrus.Errorf("verification fetch failed: %v", err)
		return
	}

	// A login wall is both marked and short. Checking the marker alone gives
	// false positives: a logged-in page can still ship a login widget in its
	// footer, and the challenge is only served to a browser that has no
	// session.
	lower := strings.ToLower(solution.HTML)
	content := flaresolverr.HTMLToMarkdown(solution.HTML)

	marked := false
	for _, marker := range []string{"biocfacade", "安全验证", "sina visitor", "登录后查看", "扫码登录", "请登录"} {
		if strings.Contains(lower, marker) {
			marked = true
			logrus.Debugf("page contains the marker %q", marker)
			break
		}
	}
	if marked && len(content) < wallContentSize {
		logrus.Errorf("the cookies do not establish a logged-in session for %s: the page is a login wall", target)
		return
	}
	logrus.Infof("verification succeeded: %s returned %d bytes of content", target, len(content))
}

// wallContentSize is the content size below which a page with a login marker
// is treated as a login wall rather than a page that merely embeds a login
// widget.
const wallContentSize = 2000

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fslogin: "+format+"\n", args...)
	os.Exit(1)
}
