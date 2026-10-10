package floaty

import "strconv"

// The word float64 in a comment, or "float" in a string, is not code.
var clean = strconv.FormatInt(12, 10) + " float64 %d"
