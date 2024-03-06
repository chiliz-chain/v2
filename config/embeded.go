package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/core"
)

func mustParseGenesisConfigFromJson(rawJson []byte) *core.Genesis {
	genesis := new(core.Genesis)
	if err := json.Unmarshal(rawJson, genesis); err != nil {
		panic(fmt.Sprintf("invalid genesis file: %v", err))
	}
	return genesis
}

//go:embed embedded/scoville.json
var scovilleRawGenesisConfig []byte

var ScovilleGenesisConfig = mustParseGenesisConfigFromJson(scovilleRawGenesisConfig)

//go:embed embedded/spicy.json
var spicyRawGenesisConfig []byte

var SpicyGenesisConfig = mustParseGenesisConfigFromJson(spicyRawGenesisConfig)

//go:embed embedded/chiliz.json
var chilizRawGenesisConfig []byte

var ChilizMainnetGenesisConfig = mustParseGenesisConfigFromJson(chilizRawGenesisConfig)
