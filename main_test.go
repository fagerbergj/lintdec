package main

import (
	"errors"
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain keeps every test away from the user's browser and OS keychain.
func TestMain(m *testing.M) {
	keyring.MockInit()
	openBrowser = func(string) error { return errors.New("tests must not open a browser") }
	os.Exit(m.Run())
}
