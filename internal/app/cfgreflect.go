package app

import (
	"fmt"
	"reflect"
)

type cfgField struct {
	key      string
	value    string
	secret   bool
	nonEmpty bool
}

// reflectValue lists the env-tagged fields of a config struct as strings.
func reflectValue(cfg any) []cfgField {
	rv := reflect.ValueOf(cfg)
	rt := rv.Type()
	var out []cfgField
	for i := range rt.NumField() {
		f := rt.Field(i)
		key := f.Tag.Get("env")
		if key == "" {
			continue
		}
		fv := rv.Field(i)
		out = append(out, cfgField{
			key: key, value: fmt.Sprint(fv.Interface()), secret: f.Tag.Get("secret") == "true", nonEmpty: !fv.IsZero(),
		})
	}
	return out
}
