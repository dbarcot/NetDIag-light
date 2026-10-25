package probe

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// Winsock error numbers (winerror.h); compared as numbers so this file
// stays free of OS packages.
const (
	wsaeAddrNotAvail syscall.Errno = 10049
	wsaeNetDown      syscall.Errno = 10050
	wsaeNetUnreach   syscall.Errno = 10051
	wsaeConnReset    syscall.Errno = 10054 // also: ICMP port unreachable answering a UDP query
	wsaeTimedOut     syscall.Errno = 10060
	wsaeConnRefused  syscall.Errno = 10061
	wsaeHostDown     syscall.Errno = 10064
	wsaeHostUnreach  syscall.Errno = 10065
)

// ClassifyNetError maps a network error to a Sample error class and a
// short detail: timeouts, name resolution failures, refused connections
// and unreachable networks are told apart; anything else is "error".
func ClassifyNetError(err error) (class, detail string) {
	var dnsErr *net.DNSError
	var errno syscall.Errno
	var nerr net.Error
	switch {
	case err == nil:
		return "", ""
	case errors.As(err, &dnsErr):
		return ClassDNS, "name not resolved: " + dnsErr.Name
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout, "timeout"
	case errors.As(err, &errno):
		switch errno {
		case wsaeConnRefused:
			return ClassRefused, "connection refused"
		case wsaeNetUnreach, wsaeHostUnreach, wsaeNetDown, wsaeHostDown, wsaeAddrNotAvail, wsaeConnReset:
			return ClassUnreachable, err.Error()
		case wsaeTimedOut:
			return ClassTimeout, "timeout"
		}
	case errors.As(err, &nerr) && nerr.Timeout():
		return ClassTimeout, "timeout"
	}
	return ClassOther, err.Error()
}
