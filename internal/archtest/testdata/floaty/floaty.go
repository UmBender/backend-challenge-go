// Package floaty is a fixture: every line below is a way a float can sneak
// into money code. The architecture test must report each one.
package floaty

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
)

var a float64 = 1  // float64 type
var b = float32(2) // float32 conversion
var c, _ = strconv.ParseFloat("1.5", 64)
var d = strconv.FormatFloat(1.5, 'f', 2, 64)
var e = math.Round(1.5) // math import
var f = new(big.Float)  // math/big import
var g, _ = json.Number("1.5").Float64()
var h = json.Number("1") // json.Number
var i = fmt.Sprintf("%.2f", 1.5)
