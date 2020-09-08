package packet

import (
	"encoding/json"

	"github.com/techx/playground/db/models"
)

// Sent by clients after receiving the init packet. Identifies them to the
// server, and in turn other clients
type JoinPacket struct {
	BasePacket
	Packet

	// Client attributes
	Name       string `json:"name,omitempty"`
	QuillToken string `json:"quillToken,omitempty"`
	Token      string `json:"token,omitempty"`

	Email string `json:"email,omitempty"`
	Code  int    `json:"code,omitempty"`

	// Server attributes
	Character *models.Character `json:"character"`
}

func NewJoinPacket(character *models.Character) *JoinPacket {
	p := new(JoinPacket)
	p.BasePacket = BasePacket{Type: "join"}
	p.Character = character
	return p
}

func (p JoinPacket) PermissionCheck(characterID string, role models.Role) bool {
	return true
}

func (p JoinPacket) MarshalBinary() ([]byte, error) {
	return json.Marshal(p)
}

func (p JoinPacket) UnmarshalBinary(data []byte) error {
	return json.Unmarshal(data, p)
}
