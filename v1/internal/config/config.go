// Package config loads and validates configuration from the environment.
//
// Environment variables only. No config files, no flag/file/env precedence
// puzzle, and no "the value came from somewhere, good luck finding where".
//
// The design document names caarlos0/env and go-playground/validator. This
// is a ~200-line reflective loader instead, for the reason the document
// itself gives two paragraphs earlier: a framework's dependency tree
// becomes every user's dependency tree, and those two libraries bring six
// modules for work that the standard library can do. The struct tags are
// deliberately the same shape, so switching to them later is mechanical.
// See docs/dependencies.md.
package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Load populates a struct from environment variables.
//
// Tags:
//
//	env:"NAME"            read from $NAME
//	env:"NAME,required"   fail if unset or empty
//	envDefault:"value"    use when unset
//	validate:"..."        oneof=a b c | gte=N | lte=N | nonempty
func Load(dst any) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return fmt.Errorf("config: Load requires a non-nil pointer to a struct, got %T", dst)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("config: Load requires a pointer to a struct, got pointer to %s", v.Kind())
	}

	t := v.Type()
	// Every problem is collected rather than returned at the first one. An
	// operator fixing a misconfigured deployment should get the whole list,
	// not discover the next missing variable on each restart.
	var problems []string

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		if field.Type.Kind() == reflect.Struct && field.Tag.Get("env") == "" &&
			field.Type != reflect.TypeOf(time.Duration(0)) {
			// Embedded or nested config struct.
			if err := Load(v.Field(i).Addr().Interface()); err != nil {
				problems = append(problems, err.Error())
			}
			continue
		}

		tag := field.Tag.Get("env")
		if tag == "" || tag == "-" {
			continue
		}
		name, required := parseEnvTag(tag)

		raw, present := os.LookupEnv(name)
		if !present || raw == "" {
			if def, ok := field.Tag.Lookup("envDefault"); ok {
				raw = def
			} else if required {
				problems = append(problems, fmt.Sprintf("%s is required but not set", name))
				continue
			} else {
				continue
			}
		}

		if err := setField(v.Field(i), raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		if rule := field.Tag.Get("validate"); rule != "" {
			if err := validate(v.Field(i), rule); err != nil {
				// Naming the variable, the value and the constraint. Not
				// "invalid config" — an operator should be able to fix it
				// from the log line without reading the source.
				problems = append(problems, fmt.Sprintf("%s %v, got %q", name, err, raw))
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

func parseEnvTag(tag string) (name string, required bool) {
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, p := range parts[1:] {
		if p == "required" {
			required = true
		}
	}
	return name, required
}

func setField(f reflect.Value, raw string) error {
	if f.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("must be a duration such as 5s or 200ms: %w", err)
		}
		f.SetInt(int64(d))
		return nil
	}

	switch f.Kind() {
	case reflect.String:
		f.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("must be true or false")
		}
		f.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		f.SetInt(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		f.SetFloat(n)
	case reflect.Slice:
		if f.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element type %s", f.Type().Elem())
		}
		var items []string
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				items = append(items, p)
			}
		}
		f.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("unsupported type %s", f.Kind())
	}
	return nil
}

func validate(f reflect.Value, rules string) error {
	for _, rule := range strings.Split(rules, ",") {
		rule = strings.TrimSpace(rule)
		switch {
		case strings.HasPrefix(rule, "oneof="):
			allowed := strings.Fields(strings.TrimPrefix(rule, "oneof="))
			got := fmt.Sprint(f.Interface())
			ok := false
			for _, a := range allowed {
				if a == got {
					ok = true
					break
				}
			}
			if !ok {
				return fmt.Errorf("must be one of [%s]", strings.Join(allowed, " "))
			}

		case strings.HasPrefix(rule, "gte="):
			bound, err := strconv.ParseFloat(strings.TrimPrefix(rule, "gte="), 64)
			if err != nil {
				return fmt.Errorf("has a malformed gte rule")
			}
			if numeric(f) < bound {
				return fmt.Errorf("must be at least %v", bound)
			}

		case strings.HasPrefix(rule, "lte="):
			bound, err := strconv.ParseFloat(strings.TrimPrefix(rule, "lte="), 64)
			if err != nil {
				return fmt.Errorf("has a malformed lte rule")
			}
			if numeric(f) > bound {
				return fmt.Errorf("must be at most %v", bound)
			}

		case rule == "nonempty":
			if f.Kind() == reflect.String && f.String() == "" {
				return fmt.Errorf("must not be empty")
			}
		}
	}
	return nil
}

func numeric(f reflect.Value) float64 {
	switch f.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(f.Int())
	case reflect.Float32, reflect.Float64:
		return f.Float()
	}
	return 0
}
