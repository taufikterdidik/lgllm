package translate

import "time"

// now is a variable so a test can pin created timestamps without sleeping.
var now = func() int64 { return time.Now().Unix() }
