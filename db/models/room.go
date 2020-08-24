package models

import (
	"encoding/json"
)

type Room struct {
	Characters map[string]*Character `json:"characters" redis:"-"`
	Elements   []*Element            `json:"elements" redis:"-"`
	Hallways   map[string]*Hallway   `json:"hallways" redis:"-"`

	Background string `json:"background" redis:"background"`
	ID         string `json:"id" redis:"id"`
	Sponsor    bool   `json:"sponsor" redis:"sponsor"`
}

func NewRoom(id, background string, sponsor bool) *Room {
	return &Room{
		Characters: map[string]*Character{},
		Elements:   []*Element{},
		Hallways:   map[string]*Hallway{},
		Background: background,
		ID:         id,
		Sponsor:    sponsor,
	}
}

func NewHomeRoom(characterID string) *Room {
	return &Room{
		Background: "home.png",
		ID:         "home:" + characterID,
		Sponsor:    false,
	}
}

func (r *Room) Init() *Room {
	r.Characters = map[string]*Character{}
	r.Elements = []*Element{}
	r.Hallways = map[string]*Hallway{}
	return r
}

func (r Room) MarshalBinary() ([]byte, error) {
	return json.Marshal(r)
}

func (r Room) UnmarshalBinary(data []byte) error {
	return json.Unmarshal(data, r)
}
