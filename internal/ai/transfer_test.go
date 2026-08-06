package ai

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestCountingClientRejectsRedirects(t *testing.T) {
	client := countingClient(time.Second, &transferCounter{})
	if client.CheckRedirect == nil {
		t.Fatal("AI client has no redirect policy")
	}
	req, err := http.NewRequest(http.MethodPost, "https://unexpected.example/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(req, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error = %v, want http.ErrUseLastResponse", err)
	}
}
