package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestEnsureDefaultRoute_ReplacesRoute(t *testing.T) {
	oldRunner := runIPRouteCommand
	oldLog := log
	t.Cleanup(func() {
		runIPRouteCommand = oldRunner
		log = oldLog
	})

	log = logrus.New()

	var gotArgs []string
	runIPRouteCommand = func(args ...string) error {
		gotArgs = append([]string(nil), args...)
		return nil
	}

	if err := ensureDefaultRoute("eth0", "10.200.1.1"); err != nil {
		t.Fatalf("ensureDefaultRoute() error = %v", err)
	}

	want := []string{"route", "replace", "default", "via", "10.200.1.1", "dev", "eth0"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("runIPRouteCommand args = %v, want %v", gotArgs, want)
	}
}

func TestEnsureDefaultRoute_ReturnsHelpfulError(t *testing.T) {
	oldRunner := runIPRouteCommand
	oldLog := log
	t.Cleanup(func() {
		runIPRouteCommand = oldRunner
		log = oldLog
	})

	log = logrus.New()

	runIPRouteCommand = func(args ...string) error {
		return errors.New("replace failed")
	}

	err := ensureDefaultRoute("eth0", "10.200.1.1")
	if err == nil {
		t.Fatal("ensureDefaultRoute() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "10.200.1.1") || !strings.Contains(err.Error(), "eth0") {
		t.Fatalf("ensureDefaultRoute() error = %v, want gateway and iface context", err)
	}
}
