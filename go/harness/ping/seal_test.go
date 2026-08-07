package ping

import "reflect"

// typeMethod reports whether a value's type (or its pointer) has a method.
//
// Both method sets are checked because a renderer would naturally be written on
// either receiver, and `fmt` reaches the value one.
func typeMethod(v any, name string) (reflect.Method, bool) {
	ty := reflect.TypeOf(v)
	if m, ok := ty.MethodByName(name); ok {
		return m, true
	}
	return reflect.PointerTo(ty).MethodByName(name)
}
