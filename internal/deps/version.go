package deps

import (
	"strconv"
	"strings"
)

// compareVersions orders Maven versions closely enough to pick the newest
// one in a local repository: numeric parts compare as numbers, and a
// pre-release qualifier (alpha, beta, milestone, rc, snapshot) sorts before
// the release it precedes, which equals ga, final and release. Unknown
// qualifiers sort after the release, lexically.
func compareVersions(a, b string) int {
	ta, tb := versionTokens(a), versionTokens(b)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		var x, y vtoken
		if i < len(ta) {
			x = ta[i]
		} else {
			x = padding(tb[i])
		}
		if i < len(tb) {
			y = tb[i]
		} else {
			y = padding(ta[i])
		}
		if c := x.compare(y); c != 0 {
			return c
		}
	}
	return 0
}

type vtoken struct {
	num  int64
	rank int // for qualifiers: see qualifierRank; numbers are rank 100
	text string
}

const numberRank = 100

var qualifierRank = map[string]int{"alpha": 1, "a": 1, "beta": 2, "b": 2, "milestone": 3, "m": 3,
	"rc": 4, "cr": 4, "snapshot": 5, "": 6, "ga": 6, "final": 6, "release": 6, "sp": 7}

// padding stands in for a missing token: zero against a number, the
// release against a qualifier.
func padding(other vtoken) vtoken {
	if other.rank == numberRank {
		return vtoken{rank: numberRank}
	}
	return vtoken{rank: 6}
}

func (x vtoken) compare(y vtoken) int {
	if x.rank != y.rank {
		if x.rank < y.rank {
			return -1
		}
		return 1
	}
	if x.rank == numberRank {
		switch {
		case x.num < y.num:
			return -1
		case x.num > y.num:
			return 1
		}
		return 0
	}
	return strings.Compare(x.text, y.text)
}

func versionTokens(v string) []vtoken {
	var out []vtoken
	v = strings.ToLower(v)
	i := 0
	for i < len(v) {
		c := v[i]
		if c == '.' || c == '-' || c == '_' {
			i++
			continue
		}
		j := i
		digit := c >= '0' && c <= '9'
		for j < len(v) && v[j] != '.' && v[j] != '-' && v[j] != '_' && (v[j] >= '0' && v[j] <= '9') == digit {
			j++
		}
		s := v[i:j]
		if digit {
			n, _ := strconv.ParseInt(s, 10, 64)
			out = append(out, vtoken{num: n, rank: numberRank})
		} else if r, ok := qualifierRank[s]; ok {
			out = append(out, vtoken{rank: r, text: s})
		} else {
			out = append(out, vtoken{rank: 8, text: s})
		}
		i = j
	}
	// Trailing zeros and release qualifiers do not make a version newer:
	// 1.0 == 1 == 1.0.0.RELEASE.
	for len(out) > 0 {
		t := out[len(out)-1]
		if t.rank == numberRank && t.num == 0 || t.rank == 6 {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	return out
}
