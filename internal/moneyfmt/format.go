// Package moneyfmt formats amounts the way customer messages show them ("KES 1,180",
// "KES 1,180.50"). Shared by the worker and the events subscriber so every message reads the same.
package moneyfmt

import (
	"fmt"
	"strings"
)

// Format renders amt with thousands separators, two decimals only when there are cents, prefixed
// by the currency (KES when empty).
func Format(amt float64, currency string) string {
	if currency == "" {
		currency = "KES"
	}
	whole := int64(amt)
	frac := amt - float64(whole)
	digits := fmt.Sprintf("%d", whole)
	neg := strings.HasPrefix(digits, "-")
	if neg {
		digits = digits[1:]
	}
	var grouped strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(d)
	}
	num := grouped.String()
	if neg {
		num = "-" + num
	}
	if frac < -0.005 || frac > 0.005 {
		num = fmt.Sprintf("%s.%02d", num, int64(frac*100+0.5))
	}
	return currency + " " + num
}
