package plugin

import "testing"

func TestRequestWorkOSDeviceAuthorization(t *testing.T) {
	dev, err := requestWorkOSDeviceAuthorization(workOSClientIDProd)
	if err != nil {
		t.Fatalf("device auth: %v", err)
	}
	if dev.DeviceCode == "" || dev.UserCode == "" || dev.VerificationURI == "" {
		t.Fatalf("incomplete device auth: %+v", dev)
	}
	t.Logf("user_code=%s uri=%s", dev.UserCode, firstNonEmpty(dev.VerificationURIComplete, dev.VerificationURI))
}

func TestHandleAuthLoginStartSmoke(t *testing.T) {
	raw, err := handleAuthLoginStart([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty response")
	}
}
