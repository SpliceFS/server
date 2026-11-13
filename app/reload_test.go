package app

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
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

// startReloading starts the fixture. The server reports each applied configuration on the
// applied channel.
func startReloading(t *testing.T, fix *fixture) *reloading {
	t.Helper()
	reloader := newReloading()
	return reloader.startWith(t, fix, func(cfg *config.Config) { reloader.applied <- cfg })
}

// startReloadingWithoutApplied starts the fixture with Options.Applied left nil.
func startReloadingWithoutApplied(t *testing.T, fix *fixture) *reloading {
	t.Helper()
	return newReloading().startWith(t, fix, nil)
}

func newReloading() *reloading {
	return &reloading{
		signals: make(chan os.Signal),
		loads:   make(chan loaded, 1),
		applied: make(chan *config.Config, 16),
	}
}

// startWith starts the fixture with the reload options that the channels of the reloading
// feed, and with applied as Options.Applied.
func (reloader *reloading) startWith(
	t *testing.T, fix *fixture, applied func(*config.Config),
) *reloading {
	t.Helper()
	reloader.running = startWith(t, fix, func(opts *Options) {
		opts.Reload = reloader.signals
		opts.Load = func() (*config.Config, error) {
			select {
			case next := <-reloader.loads:
				return next.cfg, next.err
			case <-time.After(testTimeout):
				return nil, errors.New("the test did not provide a configuration")
			}
		}
		opts.Applied = applied
	})
	return reloader
}

// send makes the server load next.
func (reloader *reloading) send(t *testing.T, next loaded) {
	t.Helper()
	reloader.loads <- next
	select {
	case reloader.signals <- syscall.SIGHUP:
	case <-time.After(testTimeout):
		t.Fatal("the server did not take the reload signal")
	}
}

// apply reloads to cfg and returns when the server has applied it.
func (reloader *reloading) apply(t *testing.T, cfg *config.Config) {
	t.Helper()
	reloader.send(t, loaded{cfg: cfg})
	select {
	case got := <-reloader.applied:
		if got != cfg {
			t.Fatalf("Applied got %p, want the loaded configuration %p", got, cfg)
		}
	case <-time.After(testTimeout):
		t.Fatalf("the configuration was not applied in time:\n%s", reloader.logs)
	}
}

// refuse reloads to next and returns when the server has logged that it did not apply it.
func (reloader *reloading) refuse(t *testing.T, next loaded) {
	t.Helper()
	reloader.send(t, next)
	reloader.refused++
	reloader.logs.waitFor(t, "configuration not applied", reloader.refused)
	select {
	case <-reloader.applied:
		t.Error("Applied was called for a configuration that was refused")
	default:
	}
}

// addAdmin adds a user who may use every share, so that a test can ask for a share without
// knowing which ones the configuration has at that moment.
func addAdmin(t *testing.T, fix *fixture) {
	t.Helper()
	admin := "password_hash: \"" + lowCostHash("right-password") + "\"\nshares: [\"*\"]\n"
	write(t, fix.data.usersDir, "carol.yaml", admin)
}

// apiGet logs in as the user from addAdmin and returns the response to GET path.
func apiGet(t *testing.T, base, path string) response {
	t.Helper()
	httpClient := client(nil)
	httpClient.Timeout = testTimeout
	login := `{"user":"carol","password":"right-password"}`
	loginResponse := call(t, httpClient, http.MethodPost, base+"/api/v1/login", "", nil, login)
	var token wire.LoginResponse
	decode(t, loginResponse.body, &token)
	return call(t, httpClient, http.MethodGet, base+"/api"+path, token.Token, nil, "")
}

// expectShares fails the test unless the server at base answers for the media share and
// the extra share with the given statuses.
func expectShares(t *testing.T, base string, media, extra int) {
	t.Helper()
	if got := apiGet(t, base, "/v1/tree/media/").StatusCode; got != media {
		t.Errorf("media share: status %d, want %d", got, media)
	}
	if got := apiGet(t, base, "/v1/tree/extra/").StatusCode; got != extra {
		t.Errorf("extra share: status %d, want %d", got, extra)
	}
}

// extraShare makes a folder with one file and returns it as a share like the media one.
func extraShare(t *testing.T, fix *fixture) config.Share {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "extra")
	write(t, dir, "extra.txt", "extra files")
	extra := fix.cfg.Shares["media"]
	extra.Path = dir
	return extra
}

// reconfigured is the configuration of the fixture with other listeners and shares.
func reconfigured(fix *fixture, listen []config.Endpoint, shares map[string]config.Share,
) *config.Config {
	next := *fix.cfg
	next.Listen = listen
	next.Shares = shares
	return &next
}

// freeEndpoint is a plain listener entry on a loopback port that nothing uses now.
func freeEndpoint(t *testing.T) config.Endpoint {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, listener.Addr())
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	entry := configEndpoint(nil, true)
	entry.Port = port
	return entry
}

// probed is the answer to a probe: the status, and whether TLS carried it.
type probed struct {
	status int
	secure bool
}

// probe makes one request for the health of the server at url.
func probe(url string, pool *x509.CertPool) (probed, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return probed{}, err
	}
	httpClient := client(pool)
	httpClient.Timeout = testTimeout
	resp, err := httpClient.Do(req)
	if err != nil {
		return probed{}, err
	}
	defer closeQuietly(resp.Body)
	return probed{status: resp.StatusCode, secure: resp.TLS != nil}, nil
}

// servesPlain fails the test unless the server at address answers its health with 200,
// over plain HTTP.
func servesPlain(t *testing.T, address string) {
	t.Helper()
	serves(t, "http", address, nil)
}

// servesTLS fails the test unless the server at address answers its health with 200, over
// TLS, to a client that trusts pool.
func servesTLS(t *testing.T, address string, pool *x509.CertPool) {
	t.Helper()
	serves(t, "https", address, pool)
}

// serves fails the test unless the server at address answers its health with 200, over the
// scheme, to a client that trusts pool.
func serves(t *testing.T, scheme, address string, pool *x509.CertPool) {
	t.Helper()
	got, err := probe(scheme+"://"+address+"/api/v1/health", pool)
	wantSecure := scheme == "https"
	if err != nil || got.status != http.StatusOK || got.secure != wantSecure {
		t.Errorf("%s://%s: status %d, TLS %v, error %v; want 200 with TLS %v",
			scheme, address, got.status, got.secure, err, wantSecure)
	}
}

// refuses fails the test if the server at address answers 200 to a request with the
// given scheme and trust.
func refuses(t *testing.T, address, scheme string, pool *x509.CertPool) {
	t.Helper()
	got, err := probe(scheme+"://"+address+"/api/v1/health", pool)
	if err == nil && got.status == http.StatusOK {
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

// shareStep is one reconfiguration of the shares and the status that each share must then
// answer with.
type shareStep struct {
	name         string
	shares       map[string]config.Share
	media, extra int
}

// applyShareStep reloads the server with the shares of step and checks what they serve. The
// steps change one running server, one after the other, so they are not subtests: a subtest
// that ran alone would meet the server in another state.
func applyShareStep(t *testing.T, reloader *reloading, fix *fixture, base string, step shareStep) {
	t.Helper()
	t.Log(step.name)
	reloader.apply(t, reconfigured(fix, fix.cfg.Listen, step.shares))
	expectShares(t, base, step.media, step.extra)
	if step.extra == http.StatusOK {
		got := apiGet(t, base, "/v1/file/extra/extra.txt")
		if got.StatusCode != http.StatusOK || string(got.body) != "extra files" {
			t.Errorf("file of the added share: status %d, body %q", got.StatusCode, got.body)
		}
	}
}

func TestReloadChangesTheShares(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	addAdmin(t, fix)
	media, extra := fix.cfg.Shares["media"], extraShare(t, fix)
	reloader := startReloading(t, fix)
	base := "http://" + reloader.addrs[0].String()
	expectShares(t, base, http.StatusOK, http.StatusNotFound)

	steps := []shareStep{
		{
			"a share is added",
			map[string]config.Share{"media": media, "extra": extra},
			http.StatusOK, http.StatusOK,
		},
		{
			"a share is removed",
			map[string]config.Share{"extra": extra},
			http.StatusNotFound, http.StatusOK,
		},
		{
			"the other share is removed and the first comes back",
			map[string]config.Share{"media": media},
			http.StatusOK, http.StatusNotFound,
		},
	}
	for _, step := range steps {
		applyShareStep(t, reloader, fix, base, step)
	}
	reloader.logs.waitFor(t, "configuration applied", len(steps))
	if err := reloader.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestReloadAddsAListener(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	reloader := startReloading(t, fix)
	added := freeEndpoint(t)
	address := listenAddress(added)
	if isOpen(address) {
		t.Fatalf("%s is in use before the reload", address)
	}
	reloader.apply(t, reconfigured(fix, []config.Endpoint{fix.cfg.Listen[0], added}, fix.cfg.Shares))
	servesPlain(t, address)
	servesPlain(t, reloader.addrs[0].String())
	if err := reloader.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
	if isOpen(address) {
		t.Error("the added listener is still open after Run returned")
	}
}

func TestReloadRemovesAListenerAndKeepsTheOthers(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	fix.cfg.Listen = []config.Endpoint{fix.cfg.Listen[0], freeEndpoint(t)}
	reloader := startReloading(t, fix)
	kept, removed := reloader.addrs[0].String(), reloader.addrs[1].String()
	servesPlain(t, removed)

	reloader.apply(t, reconfigured(fix, fix.cfg.Listen[:1], fix.cfg.Shares))
	if isOpen(removed) {
		t.Errorf("%s still accepts connections", removed)
	}
	// The port the kept listener had before the reload still answers, so it is the same
	// socket and was not closed and opened again.
	servesPlain(t, kept)
	if err := reloader.stop(); err != nil {
		t.Errorf("Run = %v, a listener that was removed on purpose is not a failure", err)
	}
}

func TestReloadChangesTheTLSOfAKeptListener(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	filesA, filesB := selfSigned(t), selfSigned(t)
	reloader := startReloading(t, fix)
	address := reloader.addrs[0].String()
	reload := func(entry config.Endpoint) {
		t.Helper()
		reloader.apply(t, reconfigured(fix, []config.Endpoint{entry}, fix.cfg.Shares))
	}
	servesPlain(t, address)

	files := configEndpoint(&config.TLS{CertFile: filesA.certFile, KeyFile: filesA.keyFile}, false)
	reload(files)
	servesTLS(t, address, filesA.pool)
	refuses(t, address, "http", nil)
	refuses(t, address, "https", filesB.pool)

	// The same entry with other content in its files is a new certificate.
	copyFile(t, filesB.certFile, filesA.certFile)
	copyFile(t, filesB.keyFile, filesA.keyFile)
	reload(files)
	servesTLS(t, address, filesB.pool)
	refuses(t, address, "https", filesA.pool)

	reload(configEndpoint(nil, false))
	own := trusting(t, filepath.Join(fix.state, localDirName, tlsDirName, certFileName))
	servesTLS(t, address, own)
	refuses(t, address, "https", filesB.pool)

	reload(configEndpoint(nil, true))
	servesPlain(t, address)
	refuses(t, address, "https", own)
	if err := reloader.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestReloadKeepsServingWhenLoadFails(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	addAdmin(t, fix)
	reloader := startReloading(t, fix)
	base := "http://" + reloader.addrs[0].String()

	reloader.refuse(t, loaded{err: errors.New("configuration folder is unreadable")})
	logs := reloader.logs.String()
	if !strings.Contains(logs, "level=ERROR") ||
		!strings.Contains(logs, "configuration folder is unreadable") {
		t.Errorf("the failure is not logged:\n%s", logs)
	}
	expectShares(t, base, http.StatusOK, http.StatusNotFound)
	servesPlain(t, reloader.addrs[0].String())

	// The server still reloads after a refusal.
	reloader.apply(t, reconfigured(fix, fix.cfg.Listen, map[string]config.Share{"extra": extraShare(t, fix)}))
	expectShares(t, base, http.StatusNotFound, http.StatusOK)
	if err := reloader.stop(); err != nil {
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

// refusalPlan makes a refusal. Every configuration of a plan drops the media share and adds
// the extra one, so that a configuration that was applied in part would show.
type refusalPlan func(t *testing.T, fix *fixture, shares map[string]config.Share) refusal

// refuseMissingCertificate is a configuration whose certificate file does not exist.
func refuseMissingCertificate(t *testing.T, fix *fixture, shares map[string]config.Share) refusal {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "missing")
	entry := configEndpoint(&config.TLS{CertFile: missing, KeyFile: missing}, false)
	return refusal{
		cfg:  reconfigured(fix, []config.Endpoint{entry}, shares),
		want: "load certificate",
	}
}

// refuseAddressInUse is a configuration with an address that something else holds.
func refuseAddressInUse(t *testing.T, fix *fixture, shares map[string]config.Share) refusal {
	t.Helper()
	var listenConfig net.ListenConfig
	taken, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(taken) })
	busy := configEndpoint(nil, true)
	busy.Port = portOf(t, taken.Addr())
	// The free address comes first, so it is open when the busy one fails.
	free := freeEndpoint(t)
	listen := []config.Endpoint{fix.cfg.Listen[0], free, busy}
	return refusal{
		cfg:  reconfigured(fix, listen, shares),
		want: "listen on " + listenAddress(busy), freed: []string{listenAddress(free)},
	}
}

// refuseMissingShareFolder is a configuration with a share whose folder does not exist.
func refuseMissingShareFolder(t *testing.T, fix *fixture, shares map[string]config.Share) refusal {
	t.Helper()
	shares["zzz"] = config.Share{Path: filepath.Join(t.TempDir(), "missing")}
	free := freeEndpoint(t)
	return refusal{
		cfg:  reconfigured(fix, []config.Endpoint{fix.cfg.Listen[0], free}, shares),
		want: "zzz", freed: []string{listenAddress(free)},
	}
}

func TestReloadRefusesAConfigurationThatCannotBeApplied(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		plan refusalPlan
	}{
		{"certificate file does not exist", refuseMissingCertificate},
		{"address is already in use", refuseAddressInUse},
		{"share folder does not exist", refuseMissingShareFolder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fix := newFixture(t)
			addAdmin(t, fix)
			reloader := startReloading(t, fix)
			base := "http://" + reloader.addrs[0].String()
			got := tt.plan(t, fix, map[string]config.Share{"extra": extraShare(t, fix)})

			reloader.refuse(t, loaded{cfg: got.cfg})
			if logs := reloader.logs.String(); !strings.Contains(logs, "level=ERROR") ||
				!strings.Contains(logs, got.want) {
				t.Errorf("the log does not name %q:\n%s", got.want, logs)
			}
			for _, address := range got.freed {
				if isOpen(address) {
					t.Errorf("%s was opened for the configuration and is still open", address)
				}
			}
			servesPlain(t, reloader.addrs[0].String())
			expectShares(t, base, http.StatusOK, http.StatusNotFound)
			if err := reloader.stop(); err != nil {
				t.Errorf("Run = %v", err)
			}
		})
	}
}

func TestReloadWithoutAppliedCallback(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	addAdmin(t, fix)
	reloader := startReloadingWithoutApplied(t, fix)
	next := map[string]config.Share{"extra": extraShare(t, fix)}

	reloader.send(t, loaded{cfg: reconfigured(fix, fix.cfg.Listen, next)})
	// Without the callback the log is the only sign that the reload is done.
	reloader.logs.waitFor(t, "configuration applied", 1)
	expectShares(t, "http://"+reloader.addrs[0].String(), http.StatusNotFound, http.StatusOK)
	if err := reloader.stop(); err != nil {
		t.Errorf("Run = %v", err)
	}
}

// A listener that is going away and does not close cleanly changes nothing for the
// configuration that removed it, so the reload goes on and logs the failure.
func TestReloadLogsARemovedListenerThatDoesNotCloseCleanly(t *testing.T) {
	t.Parallel()
	fix := newFixture(t)
	logs := newSyncBuffer()
	logger := slog.New(slog.NewTextHandler(logs, nil))
	reloader := newRunner(http.NotFoundHandler(), Options{
		Load: func() (*config.Config, error) { return fix.cfg, nil },
	})
	reloader.logger = logger
	reloader.build = builder{data: layout(fix.state), version: "test", logger: logger}
	reloader.handlers = newLive(&generation{handler: http.NotFoundHandler(), closer: failingCloser{}}, logger)
	reloader.srv = newServer(reloader.handlers, logger)
	const removedAddress = "127.0.0.1:1"
	reloader.endpoints[removedAddress] = &endpoint{Listener: brokenListener{}, address: removedAddress}
	t.Cleanup(func() {
		if err := reloader.handlers.close(); err != nil {
			t.Errorf("close = %v", err)
		}
	})
	t.Cleanup(func() {
		closeQuietly(reloader.srv)
		reloader.wg.Wait()
	})

	reloader.reload(context.Background())

	got := logs.String()
	for _, want := range []string{"level=WARN", "close removed listener", "close failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q does not contain %q", got, want)
		}
	}
	if _, kept := reloader.endpoints[removedAddress]; kept {
		t.Error("the removed listener is still in the set of listeners")
	}
}

func TestStartDropsAFailureThatFollowsTheFirst(t *testing.T) {
	t.Parallel()
	reloader := newRunner(http.NotFoundHandler(), Options{})
	first, second := listenLoopback(t), listenLoopback(t)
	// Closed without retire, so each serving goroutine reports a failure. Nothing reads
	// the failures yet, so the second one meets a full channel.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	reloader.start(first)
	reloader.start(second)
	done := make(chan struct{})
	go func() {
		reloader.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("the serving goroutines did not return")
	}
	if len(reloader.failures) != 1 {
		t.Fatalf("%d failures are queued, want only the first", len(reloader.failures))
	}
	if err := <-reloader.failures; !strings.Contains(err.Error(), "serve on 127.0.0.1:") {
		t.Errorf("failure = %v", err)
	}
}
