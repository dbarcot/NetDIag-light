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

func TestChannelFromFrequencyMHz(t *testing.T) {
	t.Parallel()
	tests := map[int]int{
		2412: 1, 2437: 6, 2472: 13, 2484: 14,
		5180: 36, 5745: 149, 5885: 177,
		5935: 2, 5955: 1, 6115: 33, 7115: 233,
		1000: 0,
	}
	for f, want := range tests {
		if got := ChannelFromFrequencyMHz(f); got != want {
			t.Errorf("ChannelFromFrequencyMHz(%d) = %d, want %d", f, got, want)
		}
	}
}

func TestMatchFrequency(t *testing.T) {
	t.Parallel()
	ours := net.HardwareAddr{1, 1, 1, 1, 1, 1}
	list := []wlanBSS{
		{BSSID: net.HardwareAddr{2, 2, 2, 2, 2, 2}, FrequencyMHz: 2412}, // ch 1, 2.4 GHz
		{BSSID: ours, FrequencyMHz: 5180},                               // ch 36
		{BSSID: net.HardwareAddr{3, 3, 3, 3, 3, 3}, FrequencyMHz: 5180}, // ch 36, same freq
		{BSSID: net.HardwareAddr{4, 4, 4, 4, 4, 4}, FrequencyMHz: 5955}, // ch 1, 6 GHz
	}
	tests := []struct {
		name    string
		bssid   net.HardwareAddr
		channel int
		want    int
		ok      bool
	}{
		{"by BSSID", ours, 0, 5180, true},
		{"BSSID wins over channel", ours, 1, 5180, true},
		{"BSSID not in list", net.HardwareAddr{9, 9, 9, 9, 9, 9}, 36, 0, false},
		{"by channel, unique frequency", nil, 36, 5180, true},
		{"by channel, ambiguous 2.4 vs 6 GHz", nil, 1, 0, false},
		{"by channel, none", nil, 11, 0, false},
		{"nothing to match on", nil, 0, 0, false},
	}
	for _, tt := range tests {
		got, ok := matchFrequency(list, tt.bssid, tt.channel)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: got %d, %v; want %d, %v", tt.name, got, ok, tt.want, tt.ok)
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

func TestWiFiStatus_BandLabelAndIs24GHz(t *testing.T) {
	t.Parallel()
	tests := []struct {
		band  Band
		src   BandSource
		label string
		is24  bool
	}{
		{Band24GHz, BandFromFrequency, "2.4 GHz", true},
		{Band24GHz, BandFromChannelNo, "2.4 GHz?", false},
		{Band5GHz, BandFromFrequency, "5 GHz", false},
		{Band5GHz, BandFromChannelNo, "5 GHz?", false},
		{Band6GHz, BandFromFrequency, "6 GHz", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		s := WiFiStatus{Band: tt.band, BandSource: tt.src}
		if s.BandLabel() != tt.label || s.Is24GHz() != tt.is24 {
			t.Errorf("%s/%s: label %q is24 %v; want %q %v", tt.band, tt.src, s.BandLabel(), s.Is24GHz(), tt.label, tt.is24)
		}
	}
}

// fakeWLAN is a scripted wlanAPI; a nil error field means the query works.
type fakeWLAN struct {
	ifaces                  []wlanIface
	conn                    wlanConn
	bss                     []wlanBSS
	connErr, rssiErr, chErr error
	bssErr, ifaceErr        error
	rssiVal, chVal          int
	bssCalls                int
	closed                  bool
}

func (f *fakeWLAN) interfaces() ([]wlanIface, error)    { return f.ifaces, f.ifaceErr }
func (f *fakeWLAN) connection(string) (wlanConn, error) { return f.conn, f.connErr }
func (f *fakeWLAN) rssi(string) (int, error)            { return f.rssiVal, f.rssiErr }
func (f *fakeWLAN) channel(string) (int, error)         { return f.chVal, f.chErr }
func (f *fakeWLAN) close()                              { f.closed = true }
func (f *fakeWLAN) bssList(string) ([]wlanBSS, error) {
	f.bssCalls++
	return f.bss, f.bssErr
}

func wifiWith(api *fakeWLAN) WLANWiFi {
	return WLANWiFi{open: func() (wlanAPI, error) { return api, nil }}
}

func status(t *testing.T, api *fakeWLAN) WiFiStatus {
	t.Helper()
	got, err := wifiWith(api).Status(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Status = %v, %v", got, err)
	}
	return got[0]
}

var (
	connectedIface = []wlanIface{{GUID: "{G}", Description: "Intel Wi-Fi", Connected: true}}
	ourBSSID       = net.HardwareAddr{1, 2, 3, 4, 5, 6}
	errDenied      = fmt.Errorf("WlanQueryInterface: %w", fs.ErrPermission)
)

func TestWiFi_FullDetails(t *testing.T) {
	t.Parallel()
	api := &fakeWLAN{
		ifaces:  connectedIface,
		conn:    wlanConn{SSID: "Home", BSSID: ourBSSID, SignalQuality: 78},
		rssiVal: -61, chVal: 36,
		bss: []wlanBSS{{BSSID: ourBSSID, FrequencyMHz: 5180}},
	}
	s := status(t, api)
	if s.SSID != "Home" || s.SSIDHidden || *s.SignalPct != 78 || *s.RSSI != -61 || *s.Channel != 36 ||
		*s.FrequencyMHz != 5180 || s.Band != Band5GHz || s.BandSource != BandFromFrequency || !s.HasDetails() {
		t.Errorf("unexpected status %+v", s)
	}
	if !api.closed {
		t.Error("WLAN handle not closed")
	}
}

func TestWiFi_24GHzByFrequencyIsCertain(t *testing.T) {
	t.Parallel()
	s := status(t, &fakeWLAN{
		ifaces: connectedIface,
		conn:   wlanConn{SSID: "Old", BSSID: ourBSSID, SignalQuality: 60},
		chVal:  6, rssiErr: errors.New("n/a"),
		bss: []wlanBSS{{BSSID: ourBSSID, FrequencyMHz: 2437}},
	})
	if !s.Is24GHz() || s.BandLabel() != "2.4 GHz" {
		t.Errorf("got band %s source %s", s.Band, s.BandSource)
	}
}

func TestWiFi_LocationPrivacyDeniedSSIDIsPartialResult(t *testing.T) {
	t.Parallel()
	api := &fakeWLAN{
		ifaces:  connectedIface,
		connErr: errDenied,
		rssiVal: -75, chVal: 6,
		bssErr: errDenied,
	}
	s := status(t, api)
	if !s.SSIDHidden || s.SSID != "" {
		t.Errorf("want SSIDHidden without SSID, got %+v", s)
	}
	if s.SignalPct == nil || *s.SignalPct != 50 || *s.RSSI != -75 {
		t.Errorf("signal must still be reported (from RSSI): %+v", s)
	}
	if s.Band != Band24GHz || s.BandSource != BandFromChannelNo || *s.Channel != 6 {
		t.Errorf("band/channel must still be reported from the channel: %+v", s)
	}
	if s.Is24GHz() || s.BandLabel() != "2.4 GHz?" {
		t.Error("band from channel must be uncertain: no WIFI_24GHZ, label with ?")
	}
	if api.bssCalls != 1 {
		t.Errorf("frequency must be tried with the channel when SSID is hidden (bss calls %d)", api.bssCalls)
	}
	if !s.HasDetails() {
		t.Error("partial result has details")
	}
}

func TestWiFi_HiddenSSIDFrequencyByChannel(t *testing.T) {
	t.Parallel()
	s := status(t, &fakeWLAN{
		ifaces: connectedIface, connErr: errDenied, rssiErr: errDenied, chVal: 6,
		bss: []wlanBSS{{BSSID: net.HardwareAddr{9, 9, 9, 9, 9, 9}, FrequencyMHz: 2437}},
	})
	if s.FrequencyMHz == nil || *s.FrequencyMHz != 2437 || !s.Is24GHz() {
		t.Errorf("want frequency from a unique channel match, got %+v", s)
	}
}

func TestWiFi_HiddenSSIDAmbiguousChannelStaysUncertain(t *testing.T) {
	t.Parallel()
	s := status(t, &fakeWLAN{
		ifaces: connectedIface, connErr: errDenied, rssiErr: errDenied, chVal: 1,
		bss: []wlanBSS{{FrequencyMHz: 2412}, {FrequencyMHz: 5955}},
	})
	if s.FrequencyMHz != nil || s.BandSource != BandFromChannelNo || s.Is24GHz() {
		t.Errorf("ambiguous channel must not give a frequency: %+v", s)
	}
}

func TestWiFi_NothingAvailable(t *testing.T) {
	t.Parallel()
	api := &fakeWLAN{ifaces: connectedIface, connErr: errDenied, rssiErr: errDenied, chErr: errDenied}
	s := status(t, api)
	if s.HasDetails() || !s.SSIDHidden || s.BandSource != "" {
		t.Errorf("want no details, got %+v", s)
	}
	if api.bssCalls != 0 {
		t.Error("no BSSID and no channel: BSS list must not be queried")
	}
}

func TestWiFi_BandFallsBackToChannelWithoutFrequency(t *testing.T) {
	t.Parallel()
	s := status(t, &fakeWLAN{
		ifaces:  connectedIface,
		conn:    wlanConn{SSID: "Cafe", BSSID: ourBSSID, SignalQuality: 40},
		rssiErr: errors.New("not supported"), chVal: 11, bssErr: errors.New("not in list"),
	})
	if s.Band != Band24GHz || s.BandSource != BandFromChannelNo || s.RSSI != nil || *s.SignalPct != 40 {
		t.Errorf("got %+v", s)
	}
}

func TestWiFi_DisconnectedInterfaceHasNoQueries(t *testing.T) {
	t.Parallel()
	s := status(t, &fakeWLAN{ifaces: []wlanIface{{GUID: "{G}", Connected: false}}, conn: wlanConn{SSID: "x"}})
	if s.Connected || s.HasDetails() {
		t.Errorf("got %+v", s)
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
