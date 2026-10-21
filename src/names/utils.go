package names

import (
	"errors"
	"testing"
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func checkEq[T comparable](t *testing.T, value T, expect T) {
	t.Helper()
	if value != expect {
		t.Errorf("unexpected: %v != %v", value, expect)
	}
}

func checkPanic[T any](t *testing.T, f func() T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("function did not panic")
		}
	}()

	f()
}

func checkOk[T any, U comparable](t *testing.T, parse func(T) (U, error), value T, expect U) {
	t.Helper()
	actual, err := parse(value)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	} else if actual != expect {
		t.Errorf("unexpected result: %v, expected %v", actual, expect)
	}
}

func checkErr[T any, U any](t *testing.T, parse func(T) (U, error), value T, expect error) {
	t.Helper()
	_, err := parse(value)
	if !errors.Is(err, expect) {
		t.Errorf("unexpected error: %v, expected %v", err, expect)
	}
}
