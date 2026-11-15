// Package porttest lets a test start a server on a loopback port that the test chose itself,
// and notice when something else took that port first.
package porttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
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
	return pickOne(PickMany)
}

// pickOne is Pick with the choice of ports in the caller's hands.
func pickOne(pick func(count int) ([]int, error)) (int, error) {
	ports, err := pick(1)
	if err != nil {
		return 0, err
	}
	return ports[0], nil
}

// PickMany returns count different loopback TCP ports.
func PickMany(count int) ([]int, error) {
	var listenConfig net.ListenConfig
	return pickFrom(listenConfig.Listen, count)
}

// listenFunc is net.ListenConfig.Listen.
type listenFunc func(ctx context.Context, network, address string) (net.Listener, error)

// pickFrom binds count ports to learn their numbers and releases them all again. They are all
// open until the last is chosen, which is what makes the numbers different.
func pickFrom(listen listenFunc, count int) ([]int, error) {
	listeners := make([]net.Listener, 0, count)
	ports := make([]int, 0, count)
	var errs []error
	for range count {
		listener, err := listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			errs = append(errs, err)
			break
		}
		listeners = append(listeners, listener)
		port, err := portOf(listener.Addr())
		if err != nil {
			errs = append(errs, err)
			break
		}
		ports = append(ports, port)
	}
	for _, listener := range listeners {
		errs = append(errs, listener.Close())
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("pick %d ports: %w", count, err)
	}
	return ports, nil
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
	ports, err := tryPorts(t, 1, PickMany, func(ports []int) error { return start(ports[0]) })
	if err != nil {
		t.Fatal(err)
		return 0
	}
	return ports[0]
}

// TryMany is Try for a server that needs count different ports, such as one with several
// listeners. start gets all of them, and any one of them being in use means that all of
// them are picked again.
func TryMany(t Reporter, count int, start func(ports []int) error) []int {
	t.Helper()
	ports, err := tryPorts(t, count, PickMany, start)
	if err != nil {
		t.Fatal(err)
	}
	return ports
}

// label names ports in a log line.
func label(ports []int) string {
	numbers := make([]string, 0, len(ports))
	for _, port := range ports {
		numbers = append(numbers, strconv.Itoa(port))
	}
	if len(numbers) == 1 {
		return "port " + numbers[0]
	}
	return "ports " + strings.Join(numbers, ", ")
}

// tryPorts is Try with the choice of ports in the caller's hands, and with an error to return
// where Try fails the test.
func tryPorts(
	t Reporter, count int, pick func(count int) ([]int, error), start func(ports []int) error,
) ([]int, error) {
	t.Helper()
	var inUse error
	for range attempts {
		ports, err := pick(count)
		if err != nil {
			return nil, err
		}
		err = start(ports)
		switch {
		case err == nil:
			t.Logf("%s opened", label(ports))
			return ports, nil
		case errors.Is(err, syscall.EADDRINUSE):
			t.Logf("%s in use, trying another", label(ports))
			inUse = err
		default:
			return nil, fmt.Errorf("start on %s: %w", label(ports), err)
		}
	}
	return nil, fmt.Errorf("no port opened in %d attempts, the last one failed: %w", attempts, inUse)
}
