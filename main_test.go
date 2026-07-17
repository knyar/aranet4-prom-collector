package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rigado/ble"
)

func TestWaitForEncryption(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name    string
		ctx     context.Context
		info    *ble.EncryptionChangedInfo
		wantErr string
	}{
		{
			name: "enabled",
			ctx:  context.Background(),
			info: &ble.EncryptionChangedInfo{Status: 0, Enabled: true},
		},
		{
			name:    "failed",
			ctx:     context.Background(),
			info:    &ble.EncryptionChangedInfo{Status: 6, Err: errors.New("PIN or Key Missing")},
			wantErr: "encryption failed",
		},
		{
			name:    "not enabled without error",
			ctx:     context.Background(),
			info:    &ble.EncryptionChangedInfo{Status: 0, Enabled: false},
			wantErr: "not enabled",
		},
		{
			name:    "context canceled",
			ctx:     canceled,
			wantErr: context.Canceled.Error(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := make(chan ble.EncryptionChangedInfo, 1)
			if c.info != nil {
				ch <- *c.info
			}

			errCh := make(chan error, 1)
			go func() { errCh <- waitForEncryption(c.ctx, ch) }()

			select {
			case err := <-errCh:
				if c.wantErr == "" {
					if err != nil {
						t.Fatalf("waitForEncryption: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("waitForEncryption: got %v, want error containing %q", err, c.wantErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("waitForEncryption did not return")
			}
		})
	}
}
