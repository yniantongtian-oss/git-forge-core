package main

// A logical clock. Every timestamp the forge writes is the fixed epoch plus a counter that advances once per
// use, so the state two runs of the same request sequence build is byte-identical: nothing reads the wall clock.
// The counter is atomic, so concurrent requests still each get a distinct tick.

import "sync/atomic"

// EpochSec is 2025-01-01T00:00:00Z, the day one of the suite's scenario.
const EpochSec int64 = 1_735_689_600

var clockTicks atomic.Int64

// NextTime returns the next logical time, in unix seconds (EpochSec+1 for the first call).
func NextTime() int64 {
	return EpochSec + clockTicks.Add(1)
}

// ISO formats unix seconds as an ISO-8601 UTC timestamp without milliseconds, as GitHub writes them
// (Date.prototype.toISOString with ".000Z" replaced by "Z").
func ISO(sec int64) string {
	var buf [32]byte
	return string(appendISO(buf[:0], sec, false))
}

// NextISO returns the next logical time as an ISO timestamp.
func NextISO() string {
	return ISO(NextTime())
}

// AppendISO appends ISO(sec) to dst without allocating.
func AppendISO(dst []byte, sec int64) []byte {
	return appendISO(dst, sec, false)
}

// ISOMillis is the plain Date.prototype.toISOString form, with ".000" milliseconds
// (what `new Date(sec * 1000).toISOString()` produces, e.g. for GraphQL DateTime fields).
func ISOMillis(sec int64) string {
	var buf [32]byte
	return string(appendISO(buf[:0], sec, true))
}

func appendISO(dst []byte, sec int64, millis bool) []byte {
	days := floorDiv(sec, 86400)
	rem := sec - days*86400
	year, month, day := civilFromDays(days)
	if year >= 0 && year <= 9999 {
		dst = appendDigits(dst, year, 4)
	} else {
		// toISOString's expanded-year form: a sign and six digits.
		if year < 0 {
			dst = append(dst, '-')
			year = -year
		} else {
			dst = append(dst, '+')
		}
		dst = appendDigits(dst, year, 6)
	}
	dst = append(dst, '-')
	dst = appendDigits(dst, month, 2)
	dst = append(dst, '-')
	dst = appendDigits(dst, day, 2)
	dst = append(dst, 'T')
	dst = appendDigits(dst, rem/3600, 2)
	dst = append(dst, ':')
	dst = appendDigits(dst, rem/60%60, 2)
	dst = append(dst, ':')
	dst = appendDigits(dst, rem%60, 2)
	if millis {
		dst = append(dst, ".000"...)
	}
	return append(dst, 'Z')
}

// appendDigits appends v (non-negative) zero-padded to at least width digits.
func appendDigits(dst []byte, v int64, width int) []byte {
	var tmp [20]byte
	i := len(tmp)
	for v > 0 || len(tmp)-i < width {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	return append(dst, tmp[i:]...)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// civilFromDays converts days since 1970-01-01 into a proleptic Gregorian date (Howard Hinnant's algorithm).
func civilFromDays(z int64) (year, month, day int64) {
	z += 719468
	era := floorDiv(z, 146097)
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	year = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	day = doy - (153*mp+2)/5 + 1
	if mp < 10 {
		month = mp + 3
	} else {
		month = mp - 9
	}
	if month <= 2 {
		year++
	}
	return year, month, day
}
