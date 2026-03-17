package role

import "encoding/json"

func (*Role) EventCategory() string {
	return "role"
}

// String returns a JSON representation of the Role.
func (r Role) String() string {
	ja, _ := json.Marshal(r)
	return string(ja)
}
