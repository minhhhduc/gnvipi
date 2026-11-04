package provider

import "reflect"

// Clone defensively copies every provider collection. Configuration collection
// elements are immutable values (strings, model mappings, protocol paths and
// raw JSON bytes); copying their containers isolates all current schema fields.
// Reflecting at this configuration boundary also keeps independently added
// collections isolated without serializing credentials on cache publication.
func Clone(p *Provider) Provider {
	out := *p
	value := reflect.ValueOf(&out).Elem()
	for i := range value.NumField() {
		field := value.Field(i)
		switch field.Kind() {
		case reflect.Map:
			if field.IsNil() {
				continue
			}
			copied := reflect.MakeMapWithSize(field.Type(), field.Len())
			iterator := field.MapRange()
			for iterator.Next() {
				copied.SetMapIndex(iterator.Key(), iterator.Value())
			}
			field.Set(copied)
		case reflect.Slice:
			if field.IsNil() {
				continue
			}
			copied := reflect.MakeSlice(field.Type(), field.Len(), field.Len())
			reflect.Copy(copied, field)
			field.Set(copied)
		}
	}
	return out
}
