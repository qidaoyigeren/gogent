package entity

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
)

// FlexEnabled is an API-friendly 0/1 flag that scans PostgreSQL bool or integer types.
// Some deployments use BOOLEAN (Java/Hibernate); schema_pg uses SMALLINT.
type FlexEnabled int

func (f *FlexEnabled) Scan(value interface{}) error {
	if value == nil {
		*f = 0
		return nil
	}
	switch v := value.(type) {
	case bool:
		if v {
			*f = 1
		} else {
			*f = 0
		}
	case int64:
		if v != 0 {
			*f = 1
		} else {
			*f = 0
		}
	case int32:
		if v != 0 {
			*f = 1
		} else {
			*f = 0
		}
	case int:
		if v != 0 {
			*f = 1
		} else {
			*f = 0
		}
	case float64:
		if v != 0 {
			*f = 1
		} else {
			*f = 0
		}
	case []byte:
		s := string(v)
		switch s {
		case "t", "true", "1":
			*f = 1
		case "f", "false", "0", "":
			*f = 0
		default:
			n, err := strconv.Atoi(s)
			if err != nil {
				return fmt.Errorf("FlexEnabled: scan []byte %q: %w", s, err)
			}
			if n != 0 {
				*f = 1
			} else {
				*f = 0
			}
		}
	case string:
		switch v {
		case "t", "true", "1":
			*f = 1
		case "f", "false", "0", "":
			*f = 0
		default:
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("FlexEnabled: scan string %q: %w", v, err)
			}
			if n != 0 {
				*f = 1
			} else {
				*f = 0
			}
		}
	default:
		return fmt.Errorf("FlexEnabled: cannot scan %T", value)
	}
	return nil
}

// Value writes a PostgreSQL boolean when the column is BOOLEAN (common in Hibernate-managed DBs).
func (f FlexEnabled) Value() (driver.Value, error) {
	return f != 0, nil
}

func (f FlexEnabled) MarshalJSON() ([]byte, error) {
	return json.Marshal(int(f))
}

func (f *FlexEnabled) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		if n != 0 {
			*f = 1
		} else {
			*f = 0
		}
		return nil
	}
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*f = 1
		} else {
			*f = 0
		}
		return nil
	}
	return fmt.Errorf("FlexEnabled: bad json %s", string(data))
}
