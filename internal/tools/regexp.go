package tools

import "regexp"

func mustRegexp(s string) *regexp.Regexp { return regexp.MustCompile(s) }
