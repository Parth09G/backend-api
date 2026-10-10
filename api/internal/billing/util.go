package billing

import (
	"encoding/json"
	"time"
)

type timeT = time.Time

func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }
