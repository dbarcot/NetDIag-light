package sysapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"testing"
)

func TestBandFromFrequencyMHz(t *testing.T) {
	t.Parallel()
	tests := map[int]Band{
		2412: Band24GHz, 2484: Band24GHz,
		5180: Band5GHz, 5885: Band5GHz,
		5935: Band6GHz, 5955: Band6GHz, 7115: Band6GHz,
		0: "", 900: "", 60480: "",
	}
	for f, want := range tests {
		if got := BandFromFrequencyMHz(f); got != want {
			t.Errorf("BandFromFrequencyMHz(%d) = %q, want %q", f, got, want)
		}
	}
}

func TestBandFromChannel(t *testing.T) {
	t.Parallel()
	tests := map[int]Band{
		1: Band24GHz, 6: Band24GHz, 13: Band24GHz, 14: Band24GHz,
		36: Band5GHz, 149: Band5GHz, 177: Band5GHz,
		0: "", 15: "", 200: "",
	}
	for ch, want := range tests {
		if got := BandFromChannel(ch); got != want {
			t.Errorf("BandFromChannel(%d) = %q, want %q", ch, got, want)
		}
	}
}

func TestSignalFromRSSI(t *testing.T) {
	t.Parallel()
	tests := map[int]int{-110: 0, -100: 0, -75: 50, -50: 100, -30: 100}
	for dbm, want := range tests {
		if got := SignalFromRSSI(dbm); got != want {
			t.Errorf("SignalFromRSSI(%d) = %d, want %d", dbm, got, want)
		}
	}
}

// fakeWLAN is a scripted wlanAPI; a nil error field means the query works.
type fakeWLAN struct {
	ifaces                  []wlanIface
	conn                    wlanConn
	connErr, rssiErr, chErr error
	freqErr, ifaceErr       error
	rssiVal, chVal, freqVal int
	closed                  bool
}

func (f *fakeWLAN) interfaces() ([]wlanIface, error)           { return f.ifaces, f.ifaceErr }
func (f *fakeWLAN) connection(string) (wlanConn, error)        { return f.conn, f.connErr }
func (f *fakeWLAN) rssi(string) (int, error)                   { return f.rssiVal, f.rssiErr }
func (f *fakeWLAN) channel(string) (int, error)                { return f.chVal, f.chErr }
func (f *fakeWLAN) frequencyMHz(string, wlanConn) (int, error) { return f.freqVal, f.freqErr }
func (f *fakeWLAN) close()                                     { f.closed = true }

func wifiWith(api *fakeWLAN) WLANWiFi {
	return WLANWiFi{open: func() (wlanAPI, error) { return api, nil }}
}

var connectedIface = []wlanIface{{GUID: "{G}", Description: "Intel Wi-Fi", Connected: true}}

func TestWiFi_FullDetails(t *testing.T) {
	t.Parallel()
	api := &fakeWLAN{
		ifaces:  connectedIface,
		conn:    wlanConn{SSID: "Home", BSSID: net.HardwareAddr{1, 2, 3, 4, 5, 6}, SignalQuality: 78},
		rssiVal: -61, chVal: 36, freqVal: 5180,
	}
	got, err := wifiWith(api).Status(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Status = %v, %v", got, err)
	}
	s := got[0]
	if s.SSID != "Home" || s.SSIDHidden || *s.SignalPct != 78 || *s.RSSI != -61 || *s.Channel != 36 ||
		*s.FrequencyMHz != 5180 || s.Band != Band5GHz || !s.HasDetails() {
		t.Errorf("unexpected status %+v", s)
	}
	if !api.closed {
		t.Error("WLAN handle not closed")
	}
}

func TestWiFi_LocationPrivacyDeniedSSIDIsPartialResult(t *testing.T) {
	t.Parallel()
	denied := fmt.Errorf("WlanQueryInterface: %w", fs.ErrPermission)
	api := &fakeWLAN{
		ifaces:  connectedIface,
		connErr: denied,
		rssiVal: -75, chVal: 6,
	}
	got, err := wifiWith(api).Status(context.Background())
	if err != nil {
		t.Fatalf("access denied must not be an error: %v", err)
	}
	s := got[0]
	if !s.SSIDHidden || s.SSID != "" {
		t.Errorf("want SSIDHidden without SSID, got %+v", s)
	}
	if s.SignalPct == nil || *s.SignalPct != 50 || *s.RSSI != -75 {
		t.Errorf("signal must still be reported (from RSSI): %+v", s)
	}
	if s.Band != Band24GHz || *s.Channel != 6 {
		t.Errorf("band/channel must still be reported: %+v", s)
	}
	if s.FrequencyMHz != nil {
		t.Error("frequency lookup needs the BSSID and must be skipped")
	}
	if !s.HasDetails() {
		t.Error("partial result has details")
	}
}

func TestWiFi_NothingAvailable(t *testing.T) {
	t.Parallel()
	denied := fs.ErrPermission
	api := &fakeWLAN{ifaces: connectedIface, connErr: denied, rssiErr: denied, chErr: denied}
	got, err := wifiWith(api).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].HasDetails() || !got[0].SSIDHidden {
		t.Errorf("want no details, got %+v", got[0])
	}
}

func TestWiFi_BandFallsBackToChannelWithoutFrequency(t *testing.T) {
	t.Parallel()
	api := &fakeWLAN{
		ifaces:  connectedIface,
		conn:    wlanConn{SSID: "Cafe", BSSID: net.HardwareAddr{1, 2, 3, 4, 5, 6}, SignalQuality: 40},
		rssiErr: errors.New("not supported"), chVal: 11, freqErr: errors.New("not in list"),
	}
	got, _ := wifiWith(api).Status(context.Background())
	s := got[0]
	if s.Band != Band24GHz || s.RSSI != nil || *s.SignalPct != 40 {
		t.Errorf("got %+v", s)
	}
}

func TestWiFi_DisconnectedInterfaceHasNoQueries(t *testing.T) {
	t.Parallel()
	api := &fakeWLAN{ifaces: []wlanIface{{GUID: "{G}", Connected: false}}, conn: wlanConn{SSID: "x"}}
	got, _ := wifiWith(api).Status(context.Background())
	if got[0].Connected || got[0].HasDetails() {
		t.Errorf("got %+v", got[0])
	}
}

func TestWiFi_NoWLANServiceIsCleanEmptyResult(t *testing.T) {
	t.Parallel()
	w := WLANWiFi{open: func() (wlanAPI, error) { return nil, errWLANUnavailable }}
	got, err := w.Status(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("want empty result, got %v, %v", got, err)
	}
}

func TestWiFi_EnumErrorIsReported(t *testing.T) {
	t.Parallel()
	api := &fakeWLAN{ifaceErr: errors.New("boom")}
	if _, err := wifiWith(api).Status(context.Background()); err == nil {
		t.Error("want error")
	}
	if !api.closed {
		t.Error("handle not closed on error")
	}
}
