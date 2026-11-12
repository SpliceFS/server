package app

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
	"github.com/SpliceFS/server/config"
)

// loaded is what the Load function of a test returns.
type loaded struct {
	cfg *config.Config
	err error
}

// reloading is a server that a test tells to apply a new configuration. Every reload has
// the test hand over what Load returns and then wait for the outcome, so nothing in these
// tests depends on timing.
type reloading struct {
	*running
	signals chan os.Signal
	loads   chan loaded
	applied chan *config.Config
	refused int // how many refusals the test has asked for
}

// startReloading starts the fixture. With applied set, the server reports each applied
// configuration on the applied channel; without it, Options.Applied stays nil.
func startReloading(t *testing.T, f *fixture, applied bool) *reloading {
	t.Helper()
	rl := &reloading{
		signals: make(chan os.Signal),
		loads:   make(chan loaded, 1),
		applied: make(chan *config.Config, 16),
	}
	rl.running = startWith(t, f, func(opts *Options) {
		opts.Reload = rl.signals
		opts.Load = func() (*config.Config, error) {
			select {
			case next := <-rl.loads:
				return next.cfg, next.err
			case <-time.After(testTimeout):
				return nil, errors.New("the test did not provide a configuration")
			}
		}
		if applied {
			opts.Applied = func(cfg *config.Config) { rl.applied <- cfg }
		}
	})
	return rl
}

// send makes the server load next.
func (rl *reloading) send(t *testing.T, next loaded) {
	t.Helper()
	rl.loads <- next
	select {
	case rl.signals <- syscall.SIGHUP:
	case <-time.After(testTimeout):
		t.Fatal("the server did not take the reload signal")
	}
}

// apply reloads to cfg and returns when the server has applied it.
func (rl *reloading) apply(t *testing.T, cfg *config.Config) {
	t.Helper()
	rl.send(t, loaded{cfg: cfg})
	select {
	case got := <-rl.applied:
		if got != cfg {
			t.Fatalf("Applied got %p, want the loaded configuration %p", got, cfg)
		}
	case <-time.After(testTimeout):
		t.Fatalf("the configuration was not applied in time:\n%s", rl.logs)
	}
}

// refuse reloads to next and returns when the server has logged that it did not apply it.
func (rl *reloading) refuse(t *testing.T, next loaded) {
	t.Helper()
	rl.send(t, next)
	rl.refused++
	rl.logs.waitFor(t, "configuration not applied", rl.refused)
	select {
	case <-rl.applied:
		t.Error("Applied was called for a configuration that was refused")
	default:
	}
}

// addAdmin adds a user who may use every share, so that a test can ask for a share without
// knowing which ones the configuration has at that moment.
func addAdmin(t *testing.T, f *fixture) {
	t.Helper()
	admin := "password_hash: \"" + lowCostHash("right-password") + "\"\nshares: [\"*\"]\n"
	write(t, f.data.usersDir, "carol.yaml", admin)
}

// apiGet logs in as the user from addAdmin and returns the status and body of GET path.
func apiGet(t *testing.T, base, path string) (int, string) {
	t.Helper()
	c := client(nil)
	c.Timeout = testTimeout
	login := `{"user":"carol","password":"right-password"}`
	_, data := call(t, c, "POST", base+"/api/v1/login", "", nil, login)
	var token wire.LoginResponse
	decode(t, data, &token)
	resp, body := call(t, c, "GET", base+"/api"+path, token.Token, nil, "")
	return resp.StatusCode, string(body)
}

// expectShares fails the test unless the server at base answers for the media share and
// the extra share with the given statuses.
func expectShares(t *testing.T, base string, media, extra int) {
	t.Helper()
	if got, _ := apiGet(t, base, "/v1/tree/media/"); got != media {
		t.Errorf("media share: status %d, want %d", got, media)
	}
	if got, _ := apiGet(t, base, "/v1/tree/extra/"); got != extra {
		t.Errorf("extra share: status %d, want %d", got, extra)
	}
}

// extraShare makes a folder with one file and returns it as a share like the media one.
func extraShare(t *testing.T, f *fixture) config.Share {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "extra")
	write(t, dir, "extra.txt", "extra files")
	s := f.cfg.Shares["media"]
	s.Path = dir
	return s
}

// reconfigured is the configuration of the fixture with other listeners and shares.
func reconfigured(f *fixture, listen []config.Endpoint, shares map[string]config.Share,
) *config.Config {
	next := *f.cfg
	next.Listen = listen
	next.Shares = shares
	return &next
}

// freeEndpoint is a plain listener entry on a loopback port that nothing uses now.
func freeEndpoint(t *testing.T) config.Endpoint {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	entry := configEndpoint(nil, true)
	entry.Port = port
	return entry
}

// probe makes one request for the health of the server at url and reports the status and
// whether TLS carried it.
func probe(url string, pool *x509.CertPool) (status int, secure bool, err error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	c := client(pool)
	c.Timeout = testTimeout
	resp, err := c.Do(req)
	if err != nil {
		return 0, false, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.TLS != nil, nil
}

// serves fails the test unless the server at address answers its health with 200, over
// TLS or not as stated, to a client that trusts pool.
func serves(t *testing.T, address string, secure bool, pool *x509.CertPool) {
	t.Helper()
	scheme := "http"
	if secure {
		scheme = "https"
	}
	status, got, err := probe(scheme+"://"+address+"/api/v1/health", pool)
	if err != nil || status != http.StatusOK || got != secure {
		t.Errorf("%s://%s: status %d, TLS %v, error %v; want 200 with TLS %v",
			scheme, address, status, got, err, secure)
	}
}

// refuses fails the test if the server at address answers 200 to a request with the
// given scheme and trust.
func refuses(t *testing.T, address, scheme string, pool *x509.CertPool) {
	t.Helper()
	status, _, err := probe(scheme+"://"+address+"/api/v1/health", pool)
	if err == nil && status == http.StatusOK {
		t.Errorf("%s://%s was served", scheme, address)
	}
}

// copyFile replaces the content of dst with the content of src.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadChangesTheShares(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	addAdmin(t, f)
	media, extra := f.cfg.Shares["media"], extraShare(t, f)
	rl := startReloading(t, f, true)
	base := "http://" + rl.addrs[0].String()
	expectShares(t, base, http.StatusOK, http.StatusNotFound)

	steps := []struct {
		name         string
		shares       map[string]config.Share
		media, extra int
	}{
		{"a share is added", map[string]config.Share{"media": media, "extra": extra},
			http.StatusOK, http.StatusOK},
		{"a share is removed", map[string]config.Share{"extra": extra},
			http.StatusNotFound, http.StatusOK},
		{"the other share is removed and the first comes back", map[string]config.Share{"media": media},
			http.StatusOK, http.StatusNotFound},
	}
	for _, tt := range steps {
		t.Run(tt.name, func(t *testing.T) {
			rl.apply(t, reconfigured(f, f.cfg.Listen, tt.shares))
			expectShares(t, base, tt.media, tt.extra)
			if tt.extra == http.StatusOK {
				if status, body := apiGet(t, base, "/v1/file/extra/extra.txt"); status != 200 ||
					body != "extra files" {
					t.Errorf("file of the added share: status %d, body %q", status, body)
				}
			}
		})
	}
	rl.logs.waitFor(t, "configuration applied", len(steps))
	if err := rl.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestReloadAddsAListener(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rl := startReloading(t, f, true)
	added := freeEndpoint(t)
	address := listenAddress(added)
	if isOpen(address) {
		t.Fatalf("%s is in use before the reload", address)
	}
	rl.apply(t, reconfigured(f, []config.Endpoint{f.cfg.Listen[0], added}, f.cfg.Shares))
	serves(t, address, false, nil)
	serves(t, rl.addrs[0].String(), false, nil)
	if err := rl.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
	if isOpen(address) {
		t.Error("the added listener is still open after Run returned")
	}
}

func TestReloadRemovesAListenerAndKeepsTheOthers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.cfg.Listen = []config.Endpoint{f.cfg.Listen[0], freeEndpoint(t)}
	rl := startReloading(t, f, true)
	kept, removed := rl.addrs[0].String(), rl.addrs[1].String()
	serves(t, removed, false, nil)

	rl.apply(t, reconfigured(f, f.cfg.Listen[:1], f.cfg.Shares))
	if isOpen(removed) {
		t.Errorf("%s still accepts connections", removed)
	}
	// The port the kept listener had before the reload still answers, so it is the same
	// socket and was not closed and opened again.
	serves(t, kept, false, nil)
	if err := rl.stop(); err != nil {
		t.Errorf("Run = %v, a listener that was removed on purpose is not a failure", err)
	}
}

func TestReloadChangesTheTLSOfAKeptListener(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	certA, keyA, poolA := selfSigned(t)
	certB, keyB, poolB := selfSigned(t)
	rl := startReloading(t, f, true)
	address := rl.addrs[0].String()
	reload := func(entry config.Endpoint) {
		t.Helper()
		rl.apply(t, reconfigured(f, []config.Endpoint{entry}, f.cfg.Shares))
	}
	serves(t, address, false, nil)

	files := configEndpoint(&config.TLS{CertFile: certA, KeyFile: keyA}, false)
	reload(files)
	serves(t, address, true, poolA)
	refuses(t, address, "http", nil)
	refuses(t, address, "https", poolB)

	// The same entry with other content in its files is a new certificate.
	copyFile(t, certB, certA)
	copyFile(t, keyB, keyA)
	reload(files)
	serves(t, address, true, poolB)
	refuses(t, address, "https", poolA)

	reload(configEndpoint(nil, false))
	own := trusting(t, filepath.Join(f.state, localDirName, tlsDirName, certFileName))
	serves(t, address, true, own)
	refuses(t, address, "https", poolB)

	reload(configEndpoint(nil, true))
	serves(t, address, false, nil)
	refuses(t, address, "https", own)
	if err := rl.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestReloadKeepsServingWhenLoadFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	addAdmin(t, f)
	rl := startReloading(t, f, true)
	base := "http://" + rl.addrs[0].String()

	rl.refuse(t, loaded{err: errors.New("configuration folder is unreadable")})
	logs := rl.logs.String()
	if !strings.Contains(logs, "level=ERROR") ||
		!strings.Contains(logs, "configuration folder is unreadable") {
		t.Errorf("the failure is not logged:\n%s", logs)
	}
	expectShares(t, base, http.StatusOK, http.StatusNotFound)
	serves(t, rl.addrs[0].String(), false, nil)

	// The server still reloads after a refusal.
	rl.apply(t, reconfigured(f, f.cfg.Listen, map[string]config.Share{"extra": extraShare(t, f)}))
	expectShares(t, base, http.StatusNotFound, http.StatusOK)
	if err := rl.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

// refusal is a configuration that cannot be applied, the error the log must name, and the
// addresses that the attempt may have opened and must have closed again.
type refusal struct {
	cfg   *config.Config
	want  string
	freed []string
}

func TestReloadRefusesAConfigurationThatCannotBeApplied(t *testing.T) {
	t.Parallel()
	// Every configuration drops the media share and adds the extra one, so that a
	// configuration that was applied in part would show.
	tests := []struct {
		name string
		plan func(t *testing.T, f *fixture, shares map[string]config.Share) refusal
	}{
		{"certificate file does not exist", func(t *testing.T, f *fixture,
			shares map[string]config.Share) refusal {
			missing := filepath.Join(t.TempDir(), "missing")
			entry := configEndpoint(&config.TLS{CertFile: missing, KeyFile: missing}, false)
			return refusal{cfg: reconfigured(f, []config.Endpoint{entry}, shares),
				want: "load certificate"}
		}},
		{"address is already in use", func(t *testing.T, f *fixture,
			shares map[string]config.Share) refusal {
			var lc net.ListenConfig
			taken, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = taken.Close() })
			busy := configEndpoint(nil, true)
			busy.Port = taken.Addr().(*net.TCPAddr).Port
			// The free address comes first, so it is open when the busy one fails.
			free := freeEndpoint(t)
			listen := []config.Endpoint{f.cfg.Listen[0], free, busy}
			return refusal{cfg: reconfigured(f, listen, shares),
				want: "listen on " + listenAddress(busy), freed: []string{listenAddress(free)}}
		}},
		{"share folder does not exist", func(t *testing.T, f *fixture,
			shares map[string]config.Share) refusal {
			shares["zzz"] = config.Share{Path: filepath.Join(t.TempDir(), "missing")}
			free := freeEndpoint(t)
			return refusal{cfg: reconfigured(f, []config.Endpoint{f.cfg.Listen[0], free}, shares),
				want: "zzz", freed: []string{listenAddress(free)}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			addAdmin(t, f)
			rl := startReloading(t, f, true)
			base := "http://" + rl.addrs[0].String()
			got := tt.plan(t, f, map[string]config.Share{"extra": extraShare(t, f)})

			rl.refuse(t, loaded{cfg: got.cfg})
			if logs := rl.logs.String(); !strings.Contains(logs, "level=ERROR") ||
				!strings.Contains(logs, got.want) {
				t.Errorf("the log does not name %q:\n%s", got.want, logs)
			}
			for _, address := range got.freed {
				if isOpen(address) {
					t.Errorf("%s was opened for the configuration and is still open", address)
				}
			}
			serves(t, rl.addrs[0].String(), false, nil)
			expectShares(t, base, http.StatusOK, http.StatusNotFound)
			if err := rl.stop(); err != nil {
				t.Errorf("Run = %v", err)
			}
		})
	}
}

func TestReloadWithoutAppliedCallback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	addAdmin(t, f)
	rl := startReloading(t, f, false)
	next := map[string]config.Share{"extra": extraShare(t, f)}

	rl.send(t, loaded{cfg: reconfigured(f, f.cfg.Listen, next)})
	// Without the callback the log is the only sign that the reload is done.
	rl.logs.waitFor(t, "configuration applied", 1)
	expectShares(t, "http://"+rl.addrs[0].String(), http.StatusNotFound, http.StatusOK)
	if err := rl.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestStartDropsAFailureThatFollowsTheFirst(t *testing.T) {
	t.Parallel()
	r := newRunner(http.NotFoundHandler(), Options{})
	first, second := listenLoopback(t), listenLoopback(t)
	// Closed without retire, so each serving goroutine reports a failure. Nothing reads
	// the failures yet, so the second one meets a full channel.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	r.start(first)
	r.start(second)
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("the serving goroutines did not return")
	}
	if len(r.failures) != 1 {
		t.Fatalf("%d failures are queued, want only the first", len(r.failures))
	}
	if err := <-r.failures; !strings.Contains(err.Error(), "serve on 127.0.0.1:") {
		t.Errorf("failure = %v", err)
	}
}
