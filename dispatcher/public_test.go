package dispatcher

import (
	"net/http/httptest"
	"testing"
)

func TestLimiter(t *testing.T) {
	l := newLimiter()
	// The burst passes, then the client must wait; another client is not affected.
	for i := 0; i < int(publicBurst); i++ {
		release, err := l.acquire("1.2.3.4")
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		release()
	}
	if _, err := l.acquire("1.2.3.4"); err == nil {
		t.Fatal("the read after the burst passed")
	}
	if release, err := l.acquire("5.6.7.8"); err != nil {
		t.Fatalf("another client: %v", err)
	} else {
		release()
	}

	// No more than publicParallel at once.
	l = newLimiter()
	for i := 0; i < publicParallel; i++ {
		if _, err := l.acquire("client " + string(rune('a'+i))); err != nil {
			t.Fatalf("read %d at once: %v", i, err)
		}
	}
	if _, err := l.acquire("one more"); err == nil {
		t.Fatal("more reads at once than allowed")
	}
}

func TestClientAddress(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:4000"
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	if got := clientAddress(r); got != "203.0.113.5" {
		t.Errorf("direct client = %s (a forwarded header from outside is not trusted)", got)
	}
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", "10.0.0.1, 198.51.100.7")
	if got := clientAddress(r); got != "198.51.100.7" {
		t.Errorf("behind a local proxy = %s", got)
	}
}
