// Package porttest lets a test start a server on a loopback port that the test chose itself,
// and notice when something else took that port first.
package porttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// errNotTCP is the failure of a listener that does not have a TCP address.
var errNotTCP = errors.New("not a TCP address")

// attempts is how many ports Try uses before it gives up. A port is taken by someone else
// only now and then, so a handful of tries is plenty and twenty in a row means the machine
// has another problem.
const attempts = 20

// Reporter is the part of testing.TB that Try uses, which *testing.T satisfies.
type Reporter interface {
	Helper()
	Logf(format string, args ...any)
	Fatal(args ...any)
}

// Pick returns a loopback TCP port that nothing listens on at this moment. Another process
// can take it before the caller does, so a caller that starts a server on it uses Try.
func Pick() (int, error) {
	var listenConfig net.ListenConfig
	return pickFrom(listenConfig.Listen)
}

// listenFunc is net.ListenConfig.Listen.
type listenFunc func(ctx context.Context, network, address string) (net.Listener, error)

// pickFrom binds a port to learn its number and releases it again.
func pickFrom(listen listenFunc) (int, error) {
	listener, err := listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick a port: %w", err)
	}
	port, portErr := portOf(listener.Addr())
	if err := errors.Join(portErr, listener.Close()); err != nil {
		return 0, fmt.Errorf("pick a port: %w", err)
	}
	return port, nil
}

// portOf returns the port of a TCP address.
func portOf(addr net.Addr) (int, error) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("%w: %v is a %T", errNotTCP, addr, addr)
	}
	return tcp.Port, nil
}

// Try picks a port and calls start with it, and returns the port once start returned nil.
// When start fails because the port is in use, which is the one failure that picking another
// port can cure, Try logs the port and tries another. Any other failure, or twenty ports in
// use, fails the test. start has to report "address in use" as an error that wraps
// syscall.EADDRINUSE. Nothing here waits for time to pass: the next try follows the failure.
func Try(t Reporter, start func(port int) error) int {
	t.Helper()
	port, err := tryPorts(t, Pick, start)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// tryPorts is Try with the choice of port in the caller's hands, and with an error to return
// where Try fails the test.
func tryPorts(t Reporter, pickPort func() (int, error), start func(port int) error) (int, error) {
	t.Helper()
	var inUse error
	for range attempts {
		port, err := pickPort()
		if err != nil {
			return 0, err
		}
		err = start(port)
		switch {
		case err == nil:
			t.Logf("port %d opened", port)
			return port, nil
		case errors.Is(err, syscall.EADDRINUSE):
			t.Logf("port %d in use, trying another", port)
			inUse = err
		default:
			return 0, fmt.Errorf("start on port %d: %w", port, err)
		}
	}
	return 0, fmt.Errorf("no port opened in %d attempts, the last one failed: %w", attempts, inUse)
}
