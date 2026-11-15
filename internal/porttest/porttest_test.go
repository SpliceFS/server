package porttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

// recorder is a Reporter that keeps what it was told, and does not stop the test on Fatal.
type recorder struct {
	logs     []string
	fatal    []any
	errors   []string
	cleanups []func()
}

func (*recorder) Helper() {}

func (r *recorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatal(args ...any) { r.fatal = args }

func (r *recorder) Error(args ...any) { r.errors = append(r.errors, fmt.Sprint(args...)) }

func (r *recorder) Cleanup(cleanup func()) { r.cleanups = append(r.cleanups, cleanup) }

// finish runs the cleanups in the order a test does: last registered first.
func (r *recorder) finish() {
	for _, cleanup := range slices.Backward(r.cleanups) {
		cleanup()
	}
}

// scripted returns a pick that hands out the ports 1, 2, 3 and so on.
func scripted() func(count int) ([]int, error) {
	next := 0
	return func(count int) ([]int, error) {
		ports := make([]int, 0, count)
		for range count {
			next++
			ports = append(ports, next)
		}
		return ports, nil
	}
}

func listenOn(t *testing.T, address string) (net.Listener, error) {
	t.Helper()
	var listenConfig net.ListenConfig
	return listenConfig.Listen(context.Background(), "tcp", address)
}

func TestPickReturnsAPortThatCanBeUsed(t *testing.T) {
	t.Parallel()
	port, err := Pick()
	if err != nil || port <= 0 {
		t.Fatalf("Pick = %d, %v", port, err)
	}
	listener, err := listenOn(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("the picked port %d cannot be used: %v", port, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

// fakeListener is a listener that reports the address and the close result it is given.
type fakeListener struct {
	net.Listener

	addr     net.Addr
	closeErr error
}

func (f fakeListener) Addr() net.Addr { return f.addr }
func (f fakeListener) Close() error   { return f.closeErr }

func TestPickFailures(t *testing.T) {
	t.Parallel()
	listenFails := errors.New("no listening here")
	closeFails := errors.New("close failed")
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}
	tests := []struct {
		name   string
		listen listenFunc
		want   error
	}{
		{"listen fails", func(context.Context, string, string) (net.Listener, error) {
			return nil, listenFails
		}, listenFails},
		{"close fails", func(context.Context, string, string) (net.Listener, error) {
			return fakeListener{addr: tcp, closeErr: closeFails}, nil
		}, closeFails},
		{"address is not TCP", func(context.Context, string, string) (net.Listener, error) {
			return fakeListener{addr: &net.UnixAddr{Name: "x", Net: "unix"}}, nil
		}, errNotTCP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ports, err := pickFrom(tt.listen, 1)
			if err == nil || ports != nil {
				t.Fatalf("pickFrom = %v, %v, want an error", ports, err)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("error %v does not wrap %v", err, tt.want)
			}
		})
	}
}

func TestPickReportsThePortOfTheListener(t *testing.T) {
	t.Parallel()
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}
	ports, err := pickFrom(func(context.Context, string, string) (net.Listener, error) {
		return fakeListener{addr: tcp}, nil
	}, 1)
	if err != nil || !reflect.DeepEqual(ports, []int{4000}) {
		t.Errorf("pickFrom = %v, %v, want 4000", ports, err)
	}
}

// countingListener counts, in open, how many of its kind are open.
type countingListener struct {
	net.Listener

	addr net.Addr
	open *int
}

func (c countingListener) Addr() net.Addr { return c.addr }

func (c countingListener) Close() error {
	*c.open--
	return nil
}

// Two ports picked one after the other, each released at once, can be the same number. Every
// port stays open until the last is chosen, so the numbers differ.
func TestPickManyHoldsEveryPortUntilAllAreChosen(t *testing.T) {
	t.Parallel()
	open := 0
	var openAtListen []int
	listen := func(context.Context, string, string) (net.Listener, error) {
		openAtListen = append(openAtListen, open)
		open++
		return countingListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000 + open}, open: &open}, nil
	}
	ports, err := pickFrom(listen, 3)
	if err != nil || !reflect.DeepEqual(ports, []int{4001, 4002, 4003}) {
		t.Fatalf("pickFrom = %v, %v", ports, err)
	}
	if !reflect.DeepEqual(openAtListen, []int{0, 1, 2}) {
		t.Errorf("ports open at each listen = %v, want 0, 1, 2", openAtListen)
	}
	if open != 0 {
		t.Errorf("%d ports are still open", open)
	}
}

func TestPickManyClosesWhatItOpenedWhenOneFails(t *testing.T) {
	t.Parallel()
	open := 0
	listenFails := errors.New("no more ports")
	listen := func(context.Context, string, string) (net.Listener, error) {
		if open == 2 {
			return nil, listenFails
		}
		open++
		return countingListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000 + open}, open: &open}, nil
	}
	ports, err := pickFrom(listen, 3)
	if !errors.Is(err, listenFails) || ports != nil || open != 0 {
		t.Errorf("pickFrom = %v, %v with %d ports open, want %v and none open", ports, err, open, listenFails)
	}
}

func TestPickManyReturnsDistinctPortsThatCanAllBeUsed(t *testing.T) {
	t.Parallel()
	const count = 5
	ports, err := PickMany(count)
	if err != nil || len(ports) != count {
		t.Fatalf("PickMany = %v, %v", ports, err)
	}
	seen := make(map[int]bool, count)
	for _, port := range ports {
		if seen[port] {
			t.Errorf("port %d was picked twice in %v", port, ports)
		}
		seen[port] = true
		listener, err := listenOn(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("the picked port %d cannot be used: %v", port, err)
		}
		t.Cleanup(func() {
			if closeErr := listener.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
}

// inUseLogs is what Try logs for count ports, numbered from 1 by scripted, that are all in use.
func inUseLogs(count int) []string {
	logs := make([]string, 0, count)
	for port := 1; port <= count; port++ {
		logs = append(logs, fmt.Sprintf("port %d in use, trying another", port))
	}
	return logs
}

func TestTry(t *testing.T) {
	t.Parallel()
	inUse := fmt.Errorf("listen: %w", syscall.EADDRINUSE)
	other := errors.New("configuration is wrong")
	tests := []struct {
		name      string
		results   []error // what start returns, in turn; the last one repeats
		wantPorts []int
		wantErr   error
		wantCalls int
		wantLogs  []string
	}{
		{"opens at once", []error{nil}, []int{1}, nil, 1, []string{"port 1 opened"}},
		{
			"another port after one in use",
			[]error{inUse, nil},
			[]int{2},
			nil, 2,
			[]string{"port 1 in use, trying another", "port 2 opened"},
		},
		{"other error stops at once", []error{other}, nil, other, 1, nil},
		{
			"every port in use",
			[]error{inUse},
			nil, syscall.EADDRINUSE, attempts,
			inUseLogs(attempts),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			calls := 0
			start := func(int) error {
				result := tt.results[min(calls, len(tt.results)-1)]
				calls++
				return result
			}
			ports, err := tryPorts(rec, 1, scripted(), func(ports []int) error { return start(ports[0]) })
			if !reflect.DeepEqual(ports, tt.wantPorts) || !errors.Is(err, tt.wantErr) || calls != tt.wantCalls {
				t.Errorf("tryPorts = %v, %v after %d calls; want %v, %v after %d",
					ports, err, calls, tt.wantPorts, tt.wantErr, tt.wantCalls)
			}
			if !reflect.DeepEqual(rec.logs, tt.wantLogs) {
				t.Errorf("logs = %q, want %q", rec.logs, tt.wantLogs)
			}
		})
	}
}

func TestTryFailsWhenNoPortCanBePicked(t *testing.T) {
	t.Parallel()
	broken := errors.New("no loopback")
	ports, err := tryPorts(&recorder{}, 1, func(int) ([]int, error) { return nil, broken }, func([]int) error {
		t.Error("start was called without a port")
		return nil
	})
	if ports != nil || !errors.Is(err, broken) {
		t.Errorf("tryPorts = %v, %v", ports, err)
	}
}

func TestTryWithATest(t *testing.T) {
	t.Parallel()
	var got int
	port := Try(t, func(port int) error {
		got = port
		return nil
	})
	if port <= 0 || port != got {
		t.Errorf("Try = %d, start got %d", port, got)
	}
}

func TestTryFailsTheTest(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	broken := errors.New("start failed")
	Try(rec, func(int) error { return broken })
	if len(rec.fatal) != 1 {
		t.Fatalf("Fatal was called with %v", rec.fatal)
	}
	if err, ok := rec.fatal[0].(error); !ok || !errors.Is(err, broken) {
		t.Errorf("Fatal got %v, want an error that wraps %v", rec.fatal[0], broken)
	}
}

func TestTryManyGivesStartEveryPort(t *testing.T) {
	t.Parallel()
	inUse := fmt.Errorf("listen: %w", syscall.EADDRINUSE)
	rec := &recorder{}
	var given [][]int
	ports, err := tryPorts(rec, 3, scripted(), func(ports []int) error {
		given = append(given, ports)
		if len(given) == 1 {
			return inUse
		}
		return nil
	})
	wantGiven := [][]int{{1, 2, 3}, {4, 5, 6}}
	if err != nil || !reflect.DeepEqual(ports, []int{4, 5, 6}) || !reflect.DeepEqual(given, wantGiven) {
		t.Errorf("tryPorts = %v, %v after start got %v", ports, err, given)
	}
	wantLogs := []string{"ports 1, 2, 3 in use, trying another", "ports 4, 5, 6 opened"}
	if !reflect.DeepEqual(rec.logs, wantLogs) {
		t.Errorf("logs = %q, want %q", rec.logs, wantLogs)
	}
}

func TestTryManyWithATest(t *testing.T) {
	t.Parallel()
	var given []int
	ports := TryMany(t, 3, func(ports []int) error {
		given = ports
		return nil
	})
	if len(ports) != 3 || !reflect.DeepEqual(ports, given) {
		t.Errorf("TryMany = %v, start got %v", ports, given)
	}
}

func TestTryManyFailsTheTest(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	broken := errors.New("start failed")
	if ports := TryMany(rec, 2, func([]int) error { return broken }); ports != nil {
		t.Errorf("TryMany = %v after a failure", ports)
	}
	if len(rec.fatal) != 1 {
		t.Fatalf("Fatal was called with %v", rec.fatal)
	}
}

func TestPickOneFailsWhenNoPortCanBePicked(t *testing.T) {
	t.Parallel()
	broken := errors.New("no loopback")
	port, err := pickOne(func(int) ([]int, error) { return nil, broken })
	if port != 0 || !errors.Is(err, broken) {
		t.Errorf("pickOne = %d, %v", port, err)
	}
}

func TestHoldKeepsAPortOpenUntilTheTestEnds(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	port := Hold(rec)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if port <= 0 || len(rec.fatal) != 0 {
		t.Fatalf("Hold = %d, Fatal %v", port, rec.fatal)
	}
	if _, err := listenOn(t, address); !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("a bind on the held port failed with %v, want an address in use error", err)
	}
	rec.finish()
	listener, err := listenOn(t, address)
	if err != nil {
		t.Fatalf("the port is still held after the test ended: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHoldFromReportsWhyAPortCannotBeHeld(t *testing.T) {
	t.Parallel()
	taken := fmt.Errorf("listen: %w", syscall.EADDRINUSE)
	rec := &recorder{}
	err := holdFrom(rec, func(context.Context, string, string) (net.Listener, error) { return nil, taken }, 4000)
	if !errors.Is(err, taken) || len(rec.cleanups) != 0 {
		t.Errorf("holdFrom = %v with %d cleanups, want %v and none", err, len(rec.cleanups), taken)
	}
}

func TestHoldFromReportsAListenerThatDoesNotClose(t *testing.T) {
	t.Parallel()
	closeFails := errors.New("close failed")
	rec := &recorder{}
	err := holdFrom(rec, func(context.Context, string, string) (net.Listener, error) {
		return fakeListener{closeErr: closeFails}, nil
	}, 4000)
	rec.finish()
	if err != nil || !reflect.DeepEqual(rec.errors, []string{closeFails.Error()}) {
		t.Errorf("holdFrom = %v, errors %q, want the close error reported", err, rec.errors)
	}
}
