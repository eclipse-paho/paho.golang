package paho

import "github.com/netdata/paho.golang/packets"

type (
	// Unsubscribe is a representation of an MQTT unsubscribe packet
	Unsubscribe struct {
		Topics     []string
		Properties *UnsubscribeProperties
	}

	// UnsubscribeProperties is a struct of the properties that can be set
	// for a Unsubscribe packet
	UnsubscribeProperties struct {
		User map[string]string
	}
)

// Packet returns a packets library Unsubscribe from the paho Unsubscribe
// on which it is called
func (u *Unsubscribe) Packet() *packets.Unsubscribe {
	v := &packets.Unsubscribe{Topics: u.Topics}

	if u.Properties != nil {
		v.Properties = &packets.Properties{
			User: u.Properties.User,
		}
	}

	return v
}
