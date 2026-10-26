package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// now is a clock that never waits.
func now(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

var errWrongPassword = errors.New("password authentication failed")

func TestARefusedConnectionIsTriedAgainAndNoOtherIs(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	cases := map[string]struct {
		errs     []error
		attempts int
		want     error
	}{
		"admitted at once":                {nil, 1, nil},
		"admitted on the third try":       {[]error{refused, refused}, 3, nil},
		"refused for thirty seconds":      {repeat(refused, 40), refusedAttempts, syscall.ECONNREFUSED},
		"a wrong password is not retried": {[]error{errWrongPassword}, 1, errWrongPassword},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			attempts := 0
			_, err := untilAdmitted(t.Context(), func(context.Context) (*pgx.Conn, error) {
				attempts++
				if attempts <= len(tc.errs) {
					return nil, tc.errs[attempts-1]
				}
				return &pgx.Conn{}, nil
			}, now)
			if attempts != tc.attempts || (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("%d attempts, err %v", attempts, err)
			}
		})
	}
}

func TestARunThatEndsStopsTryingAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	never := func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	attempts := 0
	_, err := untilAdmitted(ctx, func(context.Context) (*pgx.Conn, error) {
		attempts++
		return nil, syscall.ECONNREFUSED
	}, never)
	if attempts != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("%d attempts, err %v", attempts, err)
	}
}

// What pgx returns for a port nothing listens on, with the connector's TLS-then-plain fallbacks,
// is a refusal the retry recognises.
func TestPgxReportsAClosedPortAsRefused(t *testing.T) {
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // a TCP listener's address
	_ = listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d user=admin password=x sslmode=prefer", port))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgx.ConnectConfig(ctx, config); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err %v", err)
	}
}

func repeat(err error, n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}
	return out
}
