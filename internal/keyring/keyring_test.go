package keyring

import (
	"errors"
	"os"
	"testing"
)

func TestAttributes(t *testing.T) {
	got := attributes("navigo", "alice@music.example.com")
	want := map[string]string{
		"application": "navigo",
		"service":     "navigo",
		"username":    "alice@music.example.com",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if l := label("bob@host"); l != "Navigo password for bob@host" {
		t.Errorf("label = %q", l)
	}
}

func TestNoBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/navigo-test-bus")
	if Available() {
		t.Fatal("Available() = true without a bus")
	}
	if _, err := Get("navigo", "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get err = %v, want ErrUnavailable", err)
	}
}

// TestRoundTrip runs only when a Secret Service is reachable and
// NAVIGO_KEYRING_TEST=1, to avoid touching a user's real keyring.
func TestRoundTrip(t *testing.T) {
	if os.Getenv("NAVIGO_KEYRING_TEST") != "1" || !Available() {
		t.Skip("secret service integration test disabled")
	}
	const svc, user = "navigo-test", "tester@localhost"
	if err := Set(svc, user, "s3cret"); err != nil {
		t.Fatal(err)
	}
	if err := Set(svc, user, "s3cret2"); err != nil {
		t.Fatal(err)
	}
	got, err := Get(svc, user)
	if err != nil || got != "s3cret2" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := Delete(svc, user); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(svc, user); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete err = %v", err)
	}
}
