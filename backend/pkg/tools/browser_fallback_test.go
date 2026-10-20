package tools

import (
	"context"
	"strings"
	"testing"

	"pentagi/pkg/tools/flaresolverr"
)

// stubFlareSolverr stands in for a live FlareSolverr instance.
type stubFlareSolverr struct {
	available bool
	solution  *flaresolverr.Solution
	err       error
	calls     int
	targets   []string
}

func (s *stubFlareSolverr) Available() bool { return s.available }

func (s *stubFlareSolverr) FetchWithRetry(_ context.Context, target string, _ int) (*flaresolverr.Solution, error) {
	s.calls++
	s.targets = append(s.targets, target)
	if s.err != nil {
		return nil, s.err
	}
	return s.solution, nil
}

func TestLooksLikeChallenge(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "baidu biometric challenge", content: `<html><script src="https://bioc-static.cdn.bcebos.com/pages/BiocFacade.js"></script></html>`, want: true},
		{name: "baidu security verification", content: `<html><title>百度安全验证</title></html>`, want: true},
		{name: "zhihu challenge", content: `<html><script>var zse-ck="x"</script></html>`, want: true},
		{name: "weibo visitor", content: `<html><title>Sina Visitor System</title></html>`, want: true},
		{name: "login wall", content: `<html><div>登录后查看更多</div></html>`, want: true},
		{name: "normal page", content: `<html><body><h1>普洱茶吧</h1><p>正常的论坛页面内容</p></body></html>`, want: false},
		{name: "empty", content: "", want: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeChallenge([]byte(tt.content)); got != tt.want {
				t.Errorf("looksLikeChallenge() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A challenge page from the primary scraper must send the request to the
// FlareSolverr fallback instead of returning the interstitial to the agent.
func TestBrowserFallsBackToFlareSolverrOnChallenge(t *testing.T) {
	t.Parallel()

	stub := &stubFlareSolverr{
		available: true,
		solution: &flaresolverr.Solution{
			HTML: `<html><body><h1>普洱茶吧</h1><p>置顶：普洱茶吧吧规</p></body></html>`,
		},
	}

	b := &browser{fs: stub}

	// The stub stands in for the fallback only; the primary scraper is not
	// reachable, so the fallback must be consulted.
	out := b.fetchViaFlareSolverr("https://tieba.baidu.com/f?kw=test", "markdown")
	if out == nil {
		t.Fatal("fallback returned nothing")
	}
	if !strings.Contains(string(out), "普洱茶吧") {
		t.Fatalf("fallback output = %q, want the forum content", out)
	}
	if stub.calls != 1 {
		t.Fatalf("fallback called %d times, want 1", stub.calls)
	}
	if stub.targets[0] != "https://tieba.baidu.com/f?kw=test" {
		t.Fatalf("fallback target = %q", stub.targets[0])
	}
}

func TestBrowserSkipsFallbackWhenUnavailable(t *testing.T) {
	t.Parallel()

	stub := &stubFlareSolverr{available: false}
	b := &browser{fs: stub}

	if out := b.fetchViaFlareSolverr("https://tieba.baidu.com/f?kw=test", "markdown"); out != nil {
		t.Fatalf("unavailable fallback returned %q, want nil", out)
	}
	if stub.calls != 0 {
		t.Fatalf("unavailable fallback was called %d times", stub.calls)
	}
}
