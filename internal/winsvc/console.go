package winsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ErrForced is returned when a second interrupt arrives while stopping.
var ErrForced = errors.New("winsvc: forced exit on second interrupt")

// RunConsole runs h as a console process: the first SIGINT or SIGTERM (on
// Windows, Ctrl+C, or closing the console) becomes a Stop event; a second
// one abandons the handler. It does not deliver power or session events.
func RunConsole(o Options, h Handler) error {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	return runConsole(o, h, sigs, time.Now)
}

func runConsole(o Options, h Handler, sigs <-chan os.Signal, now func() time.Time) error {
	o = o.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, EventBuffer)
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, events) }()

	select {
	case err := <-done:
		return err
	case <-sigs:
	}
	stop := Event{Kind: Stop, Deadline: now().Add(o.StopTimeout)}
	events <- stop // the buffer is empty: console mode queues nothing else
	cancel()

	t := time.NewTimer(o.StopTimeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-sigs:
		return ErrForced
	case <-t.C:
		return fmt.Errorf("winsvc: handler did not stop within %s", o.StopTimeout)
	}
}
