package main

import "testing"

func TestParseCPINState(t *testing.T) {
	cases := []struct {
		name     string
		resp     string
		want     string
		inserted bool
		pin      bool
		puk      bool
	}{
		{"ready", "+CPIN: READY\r\nOK", "ready", true, false, false},
		{"sim pin", "+CPIN: SIM PIN\r\nOK", "sim_pin", true, true, false},
		{"sim puk", "+CPIN: SIM PUK\r\nOK", "sim_puk", true, false, true},
		{"not inserted", "+CPIN: NOT INSERTED\r\nOK", "not_inserted", false, false, false},
		{"quoted", "+CPIN: \"SIM PIN\"\r\nOK", "sim_pin", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCPINState(tc.resp)
			if got.Normalized != tc.want {
				t.Fatalf("normalized=%q want %q", got.Normalized, tc.want)
			}
			if got.Inserted != tc.inserted {
				t.Fatalf("inserted=%v want %v", got.Inserted, tc.inserted)
			}
			if got.PinRequired != tc.pin {
				t.Fatalf("pinRequired=%v want %v", got.PinRequired, tc.pin)
			}
			if got.PukRequired != tc.puk {
				t.Fatalf("pukRequired=%v want %v", got.PukRequired, tc.puk)
			}
		})
	}
}

func TestValidateSIMPIN(t *testing.T) {
	if err := validateSIMPIN("1234"); err != nil {
		t.Fatalf("1234 should be valid: %v", err)
	}
	if err := validateSIMPIN("12"); err == nil {
		t.Fatal("expected error for short PIN")
	}
	if err := validateSIMPIN("abcd"); err == nil {
		t.Fatal("expected error for non-digit PIN")
	}
}
