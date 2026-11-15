package porttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"syscall"
	"testing"
)

// recorder is a Reporter that keeps what it was told, and does not stop the test on Fatal.
type recorder struct {
	logs  []string
	fatal []any
}

func (*recorder) Helper() {}

func (r *recorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatal(args ...any) { r.fatal = args }

// scripted returns a pickPort that hands out the ports 1, 2, 3 and so on.
func scripted() func() (int, error) {
	next := 0
	return func() (int, error) {
		next++
		return next, nil
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
			port, err := pickFrom(tt.listen)
			if err == nil || port != 0 {
				t.Fatalf("pickFrom = %d, %v, want an error", port, err)
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
	port, err := pickFrom(func(context.Context, string, string) (net.Listener, error) {
		return fakeListener{addr: tcp}, nil
	})
	if err != nil || port != 4000 {
		t.Errorf("pickFrom = %d, %v, want 4000", port, err)
	}
}

// A refused bind is what the tests that use Try pass on, so the real error has to match.
func TestARealBindFailureIsAnAddressInUseError(t *testing.T) {
	t.Parallel()
	held, err := listenOn(t, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := held.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	_, err = listenOn(t, held.Addr().String())
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("a second bind failed with %v, want an address in use error", err)
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
		wantPort  int
		wantErr   error
		wantCalls int
		wantLogs  []string
	}{
		{"opens at once", []error{nil}, 1, nil, 1, []string{"port 1 opened"}},
		{
			"another port after one in use",
			[]error{inUse, nil},
			2, nil, 2,
			[]string{"port 1 in use, trying another", "port 2 opened"},
		},
		{"other error stops at once", []error{other}, 0, other, 1, nil},
		{
			"every port in use",
			[]error{inUse},
			0, syscall.EADDRINUSE, attempts,
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
			port, err := tryPorts(rec, scripted(), start)
			if port != tt.wantPort || !errors.Is(err, tt.wantErr) || calls != tt.wantCalls {
				t.Errorf("tryPorts = %d, %v after %d calls; want %d, %v after %d",
					port, err, calls, tt.wantPort, tt.wantErr, tt.wantCalls)
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
	port, err := tryPorts(&recorder{}, func() (int, error) { return 0, broken }, func(int) error {
		t.Error("start was called without a port")
		return nil
	})
	if port != 0 || !errors.Is(err, broken) {
		t.Errorf("tryPorts = %d, %v", port, err)
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
