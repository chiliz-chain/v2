package params

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

// checkCompatible is the function that decides whether a node may keep its
// datadir when the genesis it is (re-)initialised with schedules a fork
// differently from the one the datadir was synced under. `geth init` and every
// startup path funnel through CheckCompatible -> checkCompatible: if it says
// "compatible", the node keeps the blocks it already has and applies the NEW
// schedule to them. Getting that wrong is not a crash, it is a silent rewrite
// of history — the node replays blocks it produced under schedule A with
// schedule B and diverges from the fleet at the first affected block.
//
// Measured on the branch point with `go test ./params/ -coverprofile`, the
// existing params suite reached 54.7% of checkCompatible's statements and 0% of
// CheckConfigForkOrder's; with this file's seed corpus they are 98.1% and 93.1%.
// Statement coverage is the lesser half of the story, though: COR-218
// established that NONE of the eleven Chiliz fork fields (CLAUDE.md section 4)
// is compared in checkCompatible at all, so re-running `geth init` with a moved
// snake8Time or pepper8Time is accepted without a word — a gap no amount of
// coverage of the branches that DO exist would have reported.
// FuzzCheckCompatible is the regression net for that fix — see
// chilizForksNotYetCompared below and TestChilizForksNotYetComparedIsCurrent
// for how the expected-to-fail bookkeeping flips when COR-218 lands.
//
// The properties, all evaluated on two configs that differ in at most the two
// fuzzed fields (so any verdict is attributable to those fields):
//
//  1. Reflexive: a config is compatible with an independently built copy of
//     itself, at every head. A violation means a node refuses to restart on its
//     own datadir.
//  2. Moving a fork the head has already passed (or moving one INTO the head's
//     past) is an error, for every fork field.
//  3. Moving a fork the head has not reached is allowed.
//  4. The reported rewind point is at or below the head — a rewind target above
//     the head is not a rewind, and the operator is told to do something that
//     cannot fix the mismatch.
//  5. CheckConfigForkOrder never panics, and a config it accepts has no fork
//     active at a point where the fork it follows in the ordering is not.
//     One-sided by construction, and accepted as such: the property constrains
//     what the function ACCEPTS, so an over-strict CheckConfigForkOrder — one
//     that rejects legitimate configs, which is how `geth init` breaks for an
//     operator — is invisible to it. Tightening the optional-fork bookkeeping so
//     that valid configs are refused leaves both targets in this PR green. The
//     shipped schedules are the standing guard on that direction: property 4 of
//     config.FuzzGenesisJSON re-asserts CheckConfigForkOrder on all three
//     network files on every single iteration, so a rule that rejects a real
//     config fails there.
//
// This file deliberately does not re-use isBlockForked/isForkBlockIncompatible
// to compute what it expects: the expectations are derived from plain
// comparisons here, or the target would be asserting the implementation
// against itself.

// compatForkKind is the shape of a fork field: a *big.Int block number or a
// *uint64 timestamp. It is derived from the struct field's type, not from its
// name, so a field that changes shape upstream is reclassified automatically.
type compatForkKind int

const (
	compatBlockFork compatForkKind = iota
	compatTimeFork
)

// compatFork is one fork field of ChainConfig as this target sees it.
type compatFork struct {
	// field is the ChainConfig field name. Access goes through reflection
	// (index resolved once in buildCompatForks) so that adding a fork here is
	// one line, and so that TestCompatForkTableCoversChainConfig can assert
	// this table against the struct itself.
	field string

	// what is the ConfigCompatError.What string checkCompatible reports for
	// this field, and empty for a field checkCompatible does not look at.
	// Empty is a claim about production code, not a property of the test: it
	// is what makes "this fork is silently ignored" an assertion rather than a
	// hole in the corpus.
	what string

	// cor218 marks the eleven Chiliz fork fields. They all have an empty
	// `what` today; COR-218 is the issue that fills them in.
	cor218 bool

	// exempt reports whether moving this fork past the head is nevertheless
	// allowed. Only Petersburg has one: checkCompatible lets Petersburg move
	// into the past as long as it lands exactly on the stored Constantinople
	// block, to satisfy the fork-ordering rule that Petersburg must be set
	// whenever Constantinople is.
	exempt func(stored, moved *ChainConfig, head uint64) bool

	kind  compatForkKind
	index int // resolved struct field index
}

// chilizForksNotYetCompared — EXPECTED-TO-FAIL LIST, DELETE WHEN COR-218 LANDS.
//
// These are the eleven Chiliz fork fields. checkCompatible does not compare any
// of them, so today `geth init` on a synced datadir accepts a genesis that moves
// snake8Time, pepper8Time or any of the others, and the node then applies a
// different schedule to blocks it already has. Property 2 is therefore asserted
// for the BSC fields and INVERTED for these: the target asserts that they are
// still ignored.
//
// When COR-218 lands and a field starts being compared, the inverted assertion
// fires with an instruction rather than passing quietly — see the failure
// message in FuzzCheckCompatible and the deterministic companion
// TestChilizForksNotYetComparedIsCurrent. The fix is to fill in that fork's
// `what` string in compatForkTable and drop it from here; when the last one
// goes, delete this map, the branch that reads it, and the companion test.
//
// (The list is derived from the table's cor218 flags rather than repeated, so
// the two can never drift apart.)
func chilizForksNotYetCompared() []string {
	var out []string
	for _, f := range compatForks {
		if f.cor218 && f.what == "" {
			out = append(out, f.field)
		}
	}
	return out
}

// compatForkTable lists every fork field of ChainConfig, with the What string
// checkCompatible reports for it. A field with an empty What is one that
// checkCompatible does not look at at all.
//
// TestCompatForkTableCoversChainConfig asserts this table against the struct,
// so an upstream merge that adds a fork field fails here until the fork is
// classified — which is the point: an unclassified fork field is one nobody
// has decided whether `geth init` should defend.
var compatForkTable = []compatFork{
	// ---- Ethereum block forks, all compared ----
	{field: "HomesteadBlock", what: "Homestead fork block"},
	{field: "DAOForkBlock", what: "DAO fork block"},
	{field: "EIP150Block", what: "EIP150 fork block"},
	{field: "EIP155Block", what: "EIP155 fork block"},
	{field: "EIP158Block", what: "EIP158 fork block"},
	{field: "ByzantiumBlock", what: "Byzantium fork block"},
	{field: "ConstantinopleBlock", what: "Constantinople fork block"},
	{field: "PetersburgBlock", what: "Petersburg fork block", exempt: petersburgMoveExempt},
	{field: "IstanbulBlock", what: "Istanbul fork block"},
	{field: "MuirGlacierBlock", what: "Muir Glacier fork block"},
	{field: "BerlinBlock", what: "Berlin fork block"},
	{field: "LondonBlock", what: "London fork block"},
	{field: "ArrowGlacierBlock", what: "Arrow Glacier fork block"},
	{field: "GrayGlacierBlock", what: "Gray Glacier fork block"},
	{field: "MergeNetsplitBlock", what: "Merge Start fork block"},

	// YoloV3 and Catalyst are vestigial go-ethereum devnet switches that no
	// network schedules and checkCompatible has never compared. Inherited from
	// upstream and deliberately left alone (CLAUDE.md: safe-to-override areas)
	// — listed so the completeness check stays exhaustive.
	{field: "YoloV3Block"},
	{field: "CatalystBlock"},

	// ---- BSC block forks ----
	{field: "RamanujanBlock", what: "ramanujan fork block"},
	// NielsBlock is scheduled (at 0) by all three Chiliz networks but is not
	// compared by checkCompatible upstream either. At 0 a move can only be to
	// nil or to a later block, so it is not in COR-218's scope; flagged here
	// rather than fixed, to keep this file's diff against upstream honest.
	{field: "NielsBlock"},
	{field: "MirrorSyncBlock", what: "mirrorSync fork block"},
	{field: "BrunoBlock", what: "bruno fork block"},
	{field: "EulerBlock", what: "euler fork block"},
	{field: "GibbsBlock", what: "gibbs fork block"},
	{field: "NanoBlock", what: "nano fork block"},
	{field: "MoranBlock", what: "moran fork block"},
	{field: "PlanckBlock", what: "planck fork block"},
	{field: "LubanBlock", what: "luban fork block"},
	{field: "PlatoBlock", what: "plato fork block"},
	{field: "HertzBlock", what: "hertz fork block"},
	{field: "HertzfixBlock", what: "hertzfix fork block"},

	// ---- Chiliz forks: the COR-218 set ----
	{field: "RuntimeUpgradeBlock", cor218: true},
	{field: "DeployOriginBlock", cor218: true},
	{field: "DeploymentHookFixBlock", cor218: true},
	{field: "DeployerFactoryBlock", cor218: true},
	{field: "Dragon8Time", cor218: true},
	{field: "Dragon8FixTime", cor218: true},
	{field: "Snake8Time", cor218: true},
	{field: "Snake8FixTime", cor218: true},
	{field: "Pepper8Time", cor218: true},
	{field: "Pipe8Time", cor218: true},
	{field: "DeployerProxySunsetTime", cor218: true},

	// ---- Timestamp forks, all compared ----
	{field: "ShanghaiTime", what: "Shanghai fork timestamp"},
	{field: "KeplerTime", what: "Kepler fork timestamp"},
	{field: "FeynmanTime", what: "Feynman fork timestamp"},
	{field: "FeynmanFixTime", what: "FeynmanFix fork timestamp"},
	{field: "CancunTime", what: "Cancun fork timestamp"},
	{field: "HaberTime", what: "Haber fork timestamp"},
	{field: "HaberFixTime", what: "HaberFix fork timestamp"},
	{field: "BohrTime", what: "Bohr fork timestamp"},
	{field: "PascalTime", what: "Pascal fork timestamp"},
	{field: "PragueTime", what: "Prague fork timestamp"},
	{field: "LorentzTime", what: "Lorentz fork timestamp"},
	{field: "MaxwellTime", what: "Maxwell fork timestamp"},
	{field: "FermiTime", what: "FermiTime fork timestamp"},
	{field: "OsakaTime", what: "Osaka fork timestamp"},
	{field: "MendelTime", what: "Mendel fork timestamp"},
	{field: "PasteurTime", what: "Pasteur fork timestamp"},
	{field: "VerkleTime", what: "Verkle fork timestamp"},
	{field: "BPO1Time", what: "BPO1 fork timestamp"},
	{field: "BPO2Time", what: "BPO2 fork timestamp"},
	{field: "BPO3Time", what: "BPO3 fork timestamp"},
	{field: "BPO4Time", what: "BPO4 fork timestamp"},
	{field: "BPO5Time", what: "BPO5 fork timestamp"},
	{field: "AmsterdamTime", what: "Amsterdam fork timestamp"},
}

// petersburgMoveExempt mirrors the carve-out in checkCompatible: a Petersburg
// move is accepted when the new Petersburg block equals the STORED
// Constantinople block (or when neither of those is behind the head).
// It reads the two fields directly rather than through compatForkIndexOf,
// because the table holds a reference to this function and going back through
// it would be an initialisation cycle.
func petersburgMoveExempt(stored, moved *ChainConfig, head uint64) bool {
	constantinople, constantinopleSet := compatBlockValue(stored.ConstantinopleBlock)
	petersburg, petersburgSet := compatBlockValue(moved.PetersburgBlock)
	if constantinopleSet == petersburgSet && (!constantinopleSet || constantinople == petersburg) {
		return true
	}
	return !compatActive(constantinople, constantinopleSet, head) && !compatActive(petersburg, petersburgSet, head)
}

// compatForks is compatForkTable with every field index (and kind) resolved
// against ChainConfig once, so the fuzz body does no name lookups.
var compatForks = buildCompatForks()

func buildCompatForks() []compatFork {
	typ := reflect.TypeOf(ChainConfig{})
	out := make([]compatFork, 0, len(compatForkTable))
	for _, f := range compatForkTable {
		sf, ok := typ.FieldByName(f.field)
		if !ok {
			panic("compatForkTable names ChainConfig." + f.field + ", which does not exist")
		}
		switch sf.Type {
		case reflect.TypeOf((*big.Int)(nil)):
			f.kind = compatBlockFork
		case reflect.TypeOf((*uint64)(nil)):
			f.kind = compatTimeFork
		default:
			panic("ChainConfig." + f.field + " is not a fork field (" + sf.Type.String() + ")")
		}
		f.index = sf.Index[0]
		out = append(out, f)
	}
	return out
}

func compatForkIndexOf(field string) int {
	for i, f := range compatForks {
		if f.field == field {
			return i
		}
	}
	panic("unknown fork field " + field)
}

// compatForkRead returns the value of fork i on c, and whether it is scheduled.
func compatForkRead(c *ChainConfig, i int) (uint64, bool) {
	fk := compatForks[i]
	v := reflect.ValueOf(c).Elem().Field(fk.index)
	if v.IsNil() {
		return 0, false
	}
	if fk.kind == compatBlockFork {
		return compatBlockValue(v.Interface().(*big.Int))
	}
	return *v.Interface().(*uint64), true
}

// compatBlockValue reads a block-number fork field.
func compatBlockValue(b *big.Int) (uint64, bool) {
	if b == nil {
		return 0, false
	}
	if !b.IsUint64() {
		// Not reachable from this target's value ladder; clamp rather than
		// panic so a future seed cannot turn a read into a crash.
		return math.MaxUint64, true
	}
	return b.Uint64(), true
}

// compatForkWrite sets fork i on c, or clears it when set is false.
func compatForkWrite(c *ChainConfig, i int, value uint64, set bool) {
	fk := compatForks[i]
	v := reflect.ValueOf(c).Elem().Field(fk.index)
	if fk.kind == compatBlockFork {
		v.Set(reflect.ValueOf(optBlock(set, value)))
		return
	}
	v.Set(reflect.ValueOf(optTime(set, value)))
}

// compatActive reports whether a fork scheduled at value (when set) is active
// at head. Spelled out rather than delegated to isBlockForked/isTimestampForked
// so the expectation is independent of the code under test.
func compatActive(value uint64, set bool, head uint64) bool {
	return set && value <= head
}

// compatSaturatingSub is saturatingAdd's counterpart (that helper lives in
// chiliz_forks_fuzz_test.go, COR-216).
func compatSaturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// compatLadder is the set of values a fuzzed fork field can take. Three of the
// eight are head-relative, so exact activation boundaries (head-1, head, head+1)
// are always in reach of a single byte — a fork that moves from head+1 to head
// is the smallest possible schedule change that must be refused.
func compatLadder(head uint64) [8]uint64 {
	return [8]uint64{
		0,
		1,
		head,
		compatSaturatingSub(head, 1),
		saturatingAdd(head, 1),
		13189711,   // mainnet gibbs/hertz/london height
		1760432400, // mainnet snake8Time
		math.MaxUint64,
	}
}

// Value selectors, as encoded in a fuzzed byte: below 0x40 leaves the field
// alone, 0x40..0x7f clears it (nil = never), 0x80 and above picks a ladder
// entry. Leaving the field alone must be the common case: a mutation that hits
// every field at once tells you nothing about which field caused the verdict.
const (
	compatSelKeep = 0x00
	compatSelNil  = 0x40
	compatSelVal  = 0x80
)

// compatSelValue encodes "set this field to ladder entry i".
func compatSelValue(i int) uint8 {
	return uint8(compatSelVal + i%8)
}

// compatApplySel applies one selector byte to fork i of c.
func compatApplySel(c *ChainConfig, i int, sel uint8, head, headTime uint64) {
	if sel < compatSelNil {
		return
	}
	if sel < compatSelVal {
		compatForkWrite(c, i, 0, false)
		return
	}
	h := head
	if compatForks[i].kind == compatTimeFork {
		h = headTime
	}
	ladder := compatLadder(h)
	compatForkWrite(c, i, ladder[int(sel-compatSelVal)%len(ladder)], true)
}

// ---------------------------------------------------------------------------
// Presets
// ---------------------------------------------------------------------------

// The three shipped networks, as fork schedules. They are transcribed from
// config/embedded/*.json rather than loaded from there: package config imports
// core, which imports params, so params cannot import the embedded genesis
// without a cycle (the same reason TestEmbeddedChilizForkSchedules lives in
// config). config's FuzzGenesisJSON checks the real files; these are shapes to
// fuzz around, and a drift between the two only costs realism, not correctness.
var (
	compatPresetMainnet = map[string]uint64{
		"HomesteadBlock": 0, "EIP150Block": 0, "EIP155Block": 0, "EIP158Block": 0,
		"ByzantiumBlock": 0, "ConstantinopleBlock": 0, "PetersburgBlock": 0,
		"IstanbulBlock": 0, "MuirGlacierBlock": 0,
		"RuntimeUpgradeBlock": 0, "DeployOriginBlock": 0, "DeploymentHookFixBlock": 0,
		"DeployerFactoryBlock": 5765560,
		"RamanujanBlock":       0, "NielsBlock": 0, "MirrorSyncBlock": 0, "BrunoBlock": 0,
		"GibbsBlock": 13189711, "NanoBlock": 13189711, "MoranBlock": 13189711,
		"PlanckBlock": 13189711, "HertzBlock": 13189711, "HertzfixBlock": 13189711,
		"BerlinBlock": 13189711, "LondonBlock": 13189711,
		"ArrowGlacierBlock": 13189711, "GrayGlacierBlock": 13189711,
		"ShanghaiTime": 1716300000, "KeplerTime": 1716300000,
		"Dragon8FixTime": 1718611200, "Pepper8Time": 1757410200, "Snake8Time": 1760432400,
		"CancunTime": 1782810000, "PragueTime": 1782811800, "PascalTime": 1782811800,
	}
	compatPresetSpicy = map[string]uint64{
		"HomesteadBlock": 0, "EIP150Block": 0, "EIP155Block": 0, "EIP158Block": 0,
		"ByzantiumBlock": 0, "ConstantinopleBlock": 0, "PetersburgBlock": 0,
		"IstanbulBlock": 0, "MuirGlacierBlock": 0,
		"RuntimeUpgradeBlock": 0, "DeployOriginBlock": 0, "DeploymentHookFixBlock": 0,
		"DeployerFactoryBlock": 5598587,
		"RamanujanBlock":       0, "NielsBlock": 0, "MirrorSyncBlock": 0, "BrunoBlock": 0,
		"GibbsBlock": 13614081, "NanoBlock": 13614081, "MoranBlock": 13614081,
		"PlanckBlock": 13614081, "HertzBlock": 13614081, "HertzfixBlock": 13614081,
		"BerlinBlock": 13614081, "LondonBlock": 13614081,
		"ArrowGlacierBlock": 13614081, "GrayGlacierBlock": 13614081,
		"ShanghaiTime": 1714381800, "KeplerTime": 1714381800,
		"Dragon8Time": 1714381800, "Dragon8FixTime": 1717582793, "Snake8Time": 1754991000,
		"CancunTime": 1779114600, "PragueTime": 1779116400, "PascalTime": 1779116400,
	}
	compatPresetScoville = map[string]uint64{
		"HomesteadBlock": 0, "EIP150Block": 0, "EIP155Block": 0, "EIP158Block": 0,
		"ByzantiumBlock": 0, "ConstantinopleBlock": 0, "PetersburgBlock": 0,
		"IstanbulBlock": 0, "MuirGlacierBlock": 0,
		"RuntimeUpgradeBlock": 0, "DeployOriginBlock": 2849000,
		"DeploymentHookFixBlock": 6067300, "DeployerFactoryBlock": 0,
		"RamanujanBlock": 0, "NielsBlock": 0, "MirrorSyncBlock": 0, "BrunoBlock": 0,
	}
)

// compatPresets is indexed by the fuzzed preset selector. Index 4 is the only
// non-Parlia entry: CheckConfigForkOrder returns early for a non-BSC engine
// (IsNotInBSC), a branch that would otherwise never be reached.
var compatPresets = []map[string]uint64{
	nil, // empty schedule
	compatPresetMainnet,
	compatPresetSpicy,
	compatPresetScoville,
	nil, // empty schedule, no Parlia section
}

// compatBlobSchedule is a valid schedule for every blob fork. A config that
// schedules cancun/prague/osaka/bpo*/amsterdam without one is rejected by
// CheckConfigForkOrder, so without this most fuzzed configs would never reach
// the ordering checks property 5 is about.
func compatBlobSchedule() *BlobScheduleConfig {
	blob := func() *BlobConfig { return &BlobConfig{Target: 3, Max: 6, UpdateFraction: 3338477} }
	return &BlobScheduleConfig{
		Cancun: blob(), Prague: blob(), Osaka: blob(),
		BPO1: blob(), BPO2: blob(), BPO3: blob(), BPO4: blob(), BPO5: blob(),
		Amsterdam: blob(),
	}
}

// compatBuild builds a config from a preset plus per-field overrides. It is
// called twice per fuzz case with identical arguments, so that property 1 is
// checked on two independently allocated configs rather than on one pointer
// compared with itself (which would hold trivially).
func compatBuild(preset uint8, sched []byte, blobs bool, head, headTime uint64) *ChainConfig {
	c := &ChainConfig{ChainID: big.NewInt(88888)}
	idx := int(preset) % len(compatPresets)
	if idx != 4 {
		c.Parlia = &ParliaConfig{Period: 3, Epoch: 200}
	}
	for field, value := range compatPresets[idx] {
		compatForkWrite(c, compatForkIndexOf(field), value, true)
	}
	for i := range compatForks {
		if i < len(sched) {
			compatApplySel(c, i, sched[i], head, headTime)
		}
	}
	if blobs {
		c.BlobScheduleConfig = compatBlobSchedule()
	}
	return c
}

// ---------------------------------------------------------------------------
// Fork ordering (property 5)
// ---------------------------------------------------------------------------

// compatOrderedForks mirrors the fork sequence inside CheckConfigForkOrder, in
// the same order, with the same optional flags. It is the "depends on" relation
// property 5 asserts: a config the function accepts must not have fork N active
// at a point where fork N-1 of the same kind is not.
//
// Keep in sync with CheckConfigForkOrder when an upstream merge adds a fork
// there; the entries it comments out as "fork not enabled" are omitted here for
// the same reason.
//
// ACCEPTED LIMITATION — this mirror is hand-maintained and nothing checks it is
// complete. CheckConfigForkOrder builds its sequence as a slice literal inside
// the function body, so there is no handle a test could reach for; the
// completeness check that DOES exist, TestCompatForkTableCoversChainConfig,
// catches a new fork FIELD on ChainConfig but says nothing about whether that
// fork was also inserted into the ordering walk. A BSC sync that adds a fork to
// CheckConfigForkOrder and not to this list therefore leaves property 5 quietly
// not covering it. Reviewed and accepted rather than bodged: exporting or
// reflecting on a function-local slice to close it would cost more in production
// surface than the gap is worth. An upstream merge that touches
// CheckConfigForkOrder's sequence should re-read this list by hand.
//
// The Chiliz forks are absent from BOTH lists: CheckConfigForkOrder knows only
// the BSC forks, so no ordering relation between Chiliz forks is enforced on an
// arbitrary config (COR-216 pinned this; COR-218 tracks adding them). Their
// ordering is asserted only on the shipped configs, in
// config.TestEmbeddedChilizForkSchedules — including the documented mainnet
// exception that dragon8FixTime is scheduled with dragon8Time absent, which is
// why "Dragon8Fix implies Dragon8" must never be asserted on a fuzzed config
// here even once COR-218 lands.
var compatOrderedForks = []struct {
	field    string
	optional bool
}{
	{field: "MirrorSyncBlock"},
	{field: "BrunoBlock"},
	{field: "GibbsBlock"},
	{field: "PlanckBlock"},
	{field: "HertzBlock"},
	{field: "HertzfixBlock"},
	{field: "KeplerTime"},
	{field: "CancunTime"},
	{field: "PascalTime"},
	{field: "PragueTime"},
	{field: "LorentzTime"},
	{field: "MaxwellTime"},
	{field: "FermiTime"},
	{field: "OsakaTime"},
	{field: "MendelTime"},
	{field: "PasteurTime"},
	{field: "VerkleTime", optional: true},
	{field: "BPO1Time", optional: true},
	{field: "BPO2Time", optional: true},
	{field: "BPO3Time", optional: true},
	{field: "BPO4Time", optional: true},
	{field: "BPO5Time", optional: true},
	{field: "AmsterdamTime", optional: true},
}

// checkCompatForkOrderAccepted asserts property 5 on a config
// CheckConfigForkOrder accepted, in the two halves that function enforces:
//
//   - PRESENCE: a scheduled fork never follows a non-optional fork that is not
//     scheduled at all. This is the rule CheckConfigForkOrder spells out as
//     "non-optional forks must all be present in the chain config up to the last
//     defined fork", and it holds whatever the head is and whatever kind the two
//     forks are.
//   - ACTIVATION: a fork active at (head, headTime) never follows a fork that is
//     scheduled but not yet reached, of the SAME kind. The kinds are kept apart
//     because a timestamp fork legitimately follows a block fork in the sequence
//     while being active at a point the block fork is not (genesis of a chain
//     already past the fork time).
//
// The walk mirrors CheckConfigForkOrder's own `lastFork` bookkeeping: an unset
// OPTIONAL fork is skipped and does not become the predecessor, an unset
// non-optional fork does. Skipping every unset fork instead — which is what this
// helper did before — silently promotes the last SCHEDULED fork to predecessor,
// and a regression that drops the presence rule from CheckConfigForkOrder then
// satisfies both the function and this oracle. `optional` going unread was the
// symptom.
func checkCompatForkOrderAccepted(t *testing.T, c *ChainConfig, head, headTime uint64) {
	t.Helper()
	var (
		prevName   string
		prevSet    bool
		prevActive bool
		prevKind   compatForkKind
		havePrev   bool
	)
	for _, of := range compatOrderedForks {
		i := compatForkIndexOf(of.field)
		value, set := compatForkRead(c, i)
		if of.optional && !set {
			continue
		}
		kind := compatForks[i].kind
		h := head
		if kind == compatTimeFork {
			h = headTime
		}
		active := compatActive(value, set, h)
		switch {
		case havePrev && set && !prevSet:
			t.Fatalf("CheckConfigForkOrder accepted a config where %s is scheduled but the fork it follows, %s, is not scheduled at all: %v",
				of.field, prevName, compatSchedule(c))
		case havePrev && active && prevKind == kind && !prevActive:
			t.Fatalf("CheckConfigForkOrder accepted a config where %s is active at (block %d, time %d) but the fork it follows, %s, is scheduled at a point the head has not reached: %v",
				of.field, head, headTime, prevName, compatSchedule(c))
		}
		prevName, prevSet, prevActive, prevKind, havePrev = of.field, set, active, kind, true
	}
}

// compatSchedule renders only the scheduled forks of a config, so a failure
// message stays readable (ChainConfig.String() prints every field).
func compatSchedule(c *ChainConfig) map[string]any {
	out := map[string]any{}
	for i, f := range compatForks {
		if v, set := compatForkRead(c, i); set {
			out[f.field] = v
		}
	}
	return out
}

// compatScheduleDiff renders only the forks that differ between two configs, as
// "Field: stored -> moved". Every failure in this target is about a field that
// moved, and the shipped schedules carry ~35 forks each: printing both in full
// buries the one line that matters.
func compatScheduleDiff(stored, moved *ChainConfig) string {
	var b strings.Builder
	for i, f := range compatForks {
		oldValue, oldSet := compatForkRead(stored, i)
		newValue, newSet := compatForkRead(moved, i)
		if oldSet == newSet && (!oldSet || oldValue == newValue) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %v -> %v", f.field, compatValueString(oldValue, oldSet), compatValueString(newValue, newSet))
	}
	if b.Len() == 0 {
		return "(no fork differs)"
	}
	return b.String()
}

func compatValueString(value uint64, set bool) any {
	if !set {
		return "nil"
	}
	return value
}

// ---------------------------------------------------------------------------
// The target
// ---------------------------------------------------------------------------

func FuzzCheckCompatible(f *testing.F) {
	var (
		mainnetShanghai = uint64(1716300000)
		mainnetSnake8   = uint64(1760432400)
		mainnetLondon   = uint64(13189711)
	)

	// keep is a selector slice that leaves every field of the preset alone.
	keep := make([]byte, len(compatForks))

	// One seed per fork field, moving that field and nothing else, at a head
	// past everything. Two per field, because which of the two bites depends on
	// whether the preset schedules the fork: clearing a scheduled fork always
	// changes it, and setting an unscheduled one to block/time 1 always does.
	// Generating these rather than writing them out is what makes "drop one
	// fork from checkCompatible's comparison list" fail on the SEED corpus
	// instead of only under -fuzz: every compared field has a seed of its own.
	for i := range compatForks {
		f.Add(uint8(1), keep, true, uint64(math.MaxUint64), uint64(math.MaxUint64),
			uint16(i), uint8(compatSelNil), uint16(i), uint8(compatSelKeep))
		f.Add(uint8(1), keep, true, uint64(math.MaxUint64), uint64(math.MaxUint64),
			uint16(i), compatSelValue(1), uint16(i), uint8(compatSelKeep))
	}

	// The three shipped schedules, untouched, at a head past every fork.
	for preset := uint8(1); preset <= 3; preset++ {
		f.Add(preset, keep, true, uint64(math.MaxUint64), uint64(math.MaxUint64),
			uint16(0), uint8(compatSelKeep), uint16(0), uint8(compatSelKeep))
	}

	// Head exactly at a fork boundary, and one second / one block either side.
	// A fork moved at head == forkTime-1 is legal and at head == forkTime is
	// not, so an off-by-one in the activation comparison shows up here and
	// nowhere else.
	//
	// The timestamp probe has to be a fork checkCompatible actually compares, so
	// it is mainnet's shanghaiTime and not a Chiliz one. A boundary seed built on
	// snake8Time would assert nothing about the `<=` in isTimestampForked:
	// snake8Time is one of the eleven fields checkCompatible ignores (COR-218),
	// so the case lands in the expected-to-fail branch below and expects
	// "compatible" whatever the comparison says. snake8Time keeps its own seed
	// further down, where being ignored is the point.
	//
	// The destination the fork is moved to differs per arm, and the reason is
	// the whole point of the "one before" arm. Ladder entry 4 is head+1, so at a
	// head one before a fork the preset schedules it IS the preset's own value:
	// stored and moved come out identical, the oracle's "nothing differs" guard
	// skips the field, checkCompatible is handed two equal configs and the case
	// asserts nothing whatsoever. Those two arms therefore move the fork to a
	// fixed far-future value (ladder entry 7, MaxUint64) instead, which is a real
	// move to a point the head has still not reached — and they are the only
	// seeds in this file that exercise property 3, that such a move is ALLOWED.
	// Without them, replacing isForkBlockIncompatible/isForkTimestampIncompatible
	// with a bare !configBlockEqual/!configTimestampEqual — any schedule
	// difference incompatible whatever the head, the over-strict direction that
	// breaks `geth init` on every legitimate edit to a not-yet-reached fork —
	// passes the entire seed corpus.
	shanghai := uint16(compatForkIndexOf("ShanghaiTime"))
	snake8 := uint16(compatForkIndexOf("Snake8Time"))
	london := uint16(compatForkIndexOf("LondonBlock"))
	for _, arm := range []struct {
		headTime uint64
		sel      uint8
	}{
		{mainnetShanghai - 1, compatSelValue(7)}, // not reached: moving it is allowed
		{mainnetShanghai, compatSelValue(4)},     // exactly reached: refused
		{mainnetShanghai + 1, compatSelValue(4)}, // passed: refused
	} {
		f.Add(uint8(1), keep, true, mainnetLondon, arm.headTime,
			shanghai, arm.sel, uint16(0), uint8(compatSelKeep))
	}
	for _, arm := range []struct {
		head uint64
		sel  uint8
	}{
		{mainnetLondon - 1, compatSelValue(7)}, // not reached: moving it is allowed
		{mainnetLondon, compatSelValue(4)},     // exactly reached: refused
		{mainnetLondon + 1, compatSelValue(4)}, // passed: refused
	} {
		f.Add(uint8(1), keep, true, arm.head, mainnetSnake8,
			london, arm.sel, uint16(0), uint8(compatSelKeep))
	}

	// One Chiliz fork moved forward and one moved backward, head past both.
	// This is the COR-218 case in its plainest form: today both moves are
	// accepted, and the node would apply the new schedule to blocks it already
	// has. It is also the seed that flips when COR-218 lands.
	f.Add(uint8(1), keep, true, uint64(math.MaxUint64), uint64(math.MaxUint64),
		snake8, compatSelValue(7), uint16(compatForkIndexOf("Pepper8Time")), compatSelValue(0))

	// nil versus zero, which are different: nil means "never", 0 means "since
	// genesis". Moving an unscheduled fork to 0 activates it retroactively and
	// must be refused; moving a fork scheduled at 0 to nil must be refused too.
	//
	// The second direction needs the fork to be scheduled at 0 in the FIRST
	// place, which no preset does for a timestamp fork: no Chiliz network
	// schedules bohrTime at all, so applying compatSelNil to it clears a field
	// that is already nil, stored and moved come out identical and the case
	// asserts nothing. The schedule override puts it at 0 first (the same trick
	// the Petersburg seed below uses), so the 0 -> nil move is a real one. The
	// block-fork half of the same property is the homestead seed: mainnet
	// schedules HomesteadBlock at 0, so clearing it is already a real move.
	bohr := uint16(compatForkIndexOf("BohrTime"))
	f.Add(uint8(1), keep, true, uint64(1), uint64(1), bohr, compatSelValue(0), uint16(0), uint8(compatSelKeep))
	bohrAtZero := make([]byte, len(compatForks))
	bohrAtZero[compatForkIndexOf("BohrTime")] = compatSelValue(0)
	f.Add(uint8(1), bohrAtZero, true, uint64(1), uint64(1), bohr, uint8(compatSelNil), uint16(0), uint8(compatSelKeep))
	homestead := uint16(compatForkIndexOf("HomesteadBlock"))
	f.Add(uint8(1), keep, true, uint64(1), uint64(1), homestead, uint8(compatSelNil), uint16(0), uint8(compatSelKeep))

	// The Petersburg carve-out: Petersburg is scheduled at 1 in both configs by
	// the schedule override, then moved back onto Constantinople (block 0),
	// which checkCompatible allows even though the head is past it.
	petersburgSched := make([]byte, len(compatForks))
	petersburgSched[compatForkIndexOf("PetersburgBlock")] = compatSelValue(1)
	f.Add(uint8(1), petersburgSched, true, uint64(math.MaxUint64), uint64(math.MaxUint64),
		uint16(compatForkIndexOf("PetersburgBlock")), compatSelValue(0), uint16(0), uint8(compatSelKeep))

	// Head at genesis (0/0): only forks scheduled at 0 are active, so almost
	// every move is legal — the mirror image of the seeds above, and the case
	// where a rewind target of 0 is the only correct one.
	f.Add(uint8(1), keep, true, uint64(0), uint64(0), snake8, compatSelValue(0), london, compatSelValue(0))

	// Property 5's ACTIVATION half, with a head that falls strictly between two
	// out-of-order forks: cancunTime is put one second after the head timestamp
	// and pascalTime — the fork that immediately follows it in
	// CheckConfigForkOrder's sequence — one second before it, so pascal is active
	// at a point cancun is not. CheckConfigForkOrder must reject the config, and
	// this seed is what makes the monotonic-ordering rule a plain `go test`
	// assertion: without it, deleting that rule from CheckConfigForkOrder passes
	// the whole seed corpus and is only caught under -fuzz (in about a second,
	// but only if a campaign is running).
	outOfOrderSched := make([]byte, len(compatForks))
	outOfOrderSched[compatForkIndexOf("CancunTime")] = compatSelValue(4) // headTime + 1
	outOfOrderSched[compatForkIndexOf("PascalTime")] = compatSelValue(3) // headTime - 1
	f.Add(uint8(1), outOfOrderSched, true, uint64(math.MaxUint64), uint64(1782810000),
		uint16(0), uint8(compatSelKeep), uint16(0), uint8(compatSelKeep))

	// No blob schedule (CheckConfigForkOrder rejects the shipped configs),
	// empty schedule, and the non-Parlia preset that returns early from it.
	f.Add(uint8(1), keep, false, uint64(1), uint64(1), uint16(0), uint8(compatSelKeep), uint16(0), uint8(compatSelKeep))
	f.Add(uint8(0), keep, false, uint64(0), uint64(0), uint16(0), uint8(compatSelKeep), uint16(0), uint8(compatSelKeep))
	f.Add(uint8(4), keep, false, uint64(math.MaxUint64), uint64(math.MaxUint64),
		snake8, compatSelValue(0), london, compatSelValue(0))

	f.Fuzz(func(t *testing.T,
		preset uint8, sched []byte, blobs bool, head, headTime uint64,
		moveA uint16, selA uint8, moveB uint16, selB uint8,
	) {
		stored := compatBuild(preset, sched, blobs, head, headTime)
		moved := compatBuild(preset, sched, blobs, head, headTime)
		twin := compatBuild(preset, sched, blobs, head, headTime)

		// Property 1: reflexive, at this head and at both ends of the range.
		// `twin` is built from the same inputs but shares no pointer with
		// `stored`, so this is a real comparison and not pointer identity.
		for _, probe := range [][2]uint64{{head, headTime}, {0, 0}, {math.MaxUint64, math.MaxUint64}} {
			if err := stored.checkCompatible(twin, new(big.Int).SetUint64(probe[0]), probe[1]); err != nil {
				t.Fatalf("config is incompatible with an identical copy of itself at (block %d, time %d): %v (schedule %v)",
					probe[0], probe[1], err, compatSchedule(stored))
			}
		}

		compatApplySel(moved, int(moveA)%len(compatForks), selA, head, headTime)
		compatApplySel(moved, int(moveB)%len(compatForks), selB, head, headTime)

		// Work out, from plain comparisons, what checkCompatible owes us. Only
		// the moved fields can differ, so every verdict is attributable.
		var (
			wantWhat        []string // a compared fork was moved across the head
			pendingCOR218   []string // a Chiliz fork was moved across the head
			pendingUpstream []string // an upstream-uncompared fork was moved
		)
		for i, fk := range compatForks {
			oldValue, oldSet := compatForkRead(stored, i)
			newValue, newSet := compatForkRead(moved, i)
			if oldSet == newSet && (!oldSet || oldValue == newValue) {
				continue
			}
			h := head
			if fk.kind == compatTimeFork {
				h = headTime
			}
			if !compatActive(oldValue, oldSet, h) && !compatActive(newValue, newSet, h) {
				// Property 3: neither the old nor the new schedule has been
				// reached, so the move rewrites nothing.
				continue
			}
			if fk.exempt != nil && fk.exempt(stored, moved, h) {
				continue
			}
			switch {
			case fk.what != "":
				wantWhat = append(wantWhat, fk.what)
			case fk.cor218:
				pendingCOR218 = append(pendingCOR218, fk.field)
			default:
				pendingUpstream = append(pendingUpstream, fk.field)
			}
		}

		err := stored.checkCompatible(moved, new(big.Int).SetUint64(head), headTime)

		// The order of these cases is load-bearing. A case that moves BOTH a
		// Chiliz fork and a compared BSC fork across the head has a non-empty
		// wantWhat AND a non-empty pendingCOR218 — the head-at-genesis seed and
		// the non-Parlia seed above both move snake8Time and londonBlock together
		// — and when COR-218 lands checkCompatible may well name the Chiliz fork.
		// Judging wantWhat first then reports "the only forks moved across the
		// head were [London fork block]" — never a false pass, but the wrong
		// instruction during exactly the migration the branch exists for. So the
		// "the verdict names a fork we expected" case is settled first, and an
		// unexpected verdict is attributed to the bookkeeping lists before it is
		// blamed on wantWhat.
		switch {
		case err == nil:
			// Property 2, for every fork checkCompatible claims to compare.
			if len(wantWhat) > 0 {
				t.Fatalf("moving %v with the head already at (block %d, time %d) was accepted; the node would apply the new schedule to blocks it already has\nmoved: %s",
					wantWhat, head, headTime, compatScheduleDiff(stored, moved))
			}
			// Otherwise either nothing the head has reached was moved
			// (property 3), or the only forks moved are ones checkCompatible
			// is recorded as ignoring: the eleven Chiliz fields (COR-218) and
			// the three upstream ones. Both are the expected verdict today.

		case compatWhatIn(err.What, wantWhat):
			// Property 2 satisfied: the verdict names one of the forks that was
			// moved across the head.

		case len(pendingCOR218) > 0:
			// EXPECTED-TO-FAIL BRANCH — see chilizForksNotYetCompared.
			t.Fatalf("COR-218 appears to have landed: checkCompatible now reports %q for a moved Chiliz fork (%v). "+
				"Fill in that fork's `what` string in compatForkTable and drop its cor218 flag; when the last one goes, "+
				"delete chilizForksNotYetCompared, this branch and TestChilizForksNotYetComparedIsCurrent.",
				err.What, pendingCOR218)

		case len(pendingUpstream) > 0:
			t.Fatalf("checkCompatible now reports %q for %v, which compatForkTable records as not compared. "+
				"Give that fork its `what` string there.", err.What, pendingUpstream)

		case len(wantWhat) > 0:
			t.Fatalf("checkCompatible reported %q, but the only forks moved across the head were %v (head block %d, time %d)\nmoved: %s",
				err.What, wantWhat, head, headTime, compatScheduleDiff(stored, moved))

		default:
			// Property 3: nothing that the head has reached was moved.
			t.Fatalf("checkCompatible rejected %q although no fork the head (block %d, time %d) has reached was moved\nmoved: %s",
				err.What, head, headTime, compatScheduleDiff(stored, moved))
		}

		// Property 4: a rewind target above the head is not a rewind. The
		// operator is told to roll back to a block they do not have, and the
		// mismatch that produced the error survives the rollback.
		if err != nil {
			checkCompatRewindBounds(t, err, head, headTime)
		}

		// CheckCompatible iterates checkCompatible to find the lowest conflict.
		// It must agree on whether there IS a conflict, keep its rewind targets
		// within the head, and terminate — each iteration lowers the head it
		// probes, so a rewind that failed to move would spin forever.
		iterated := stored.CheckCompatible(moved, head, headTime)
		if (iterated == nil) != (err == nil) {
			t.Fatalf("CheckCompatible returned %v but checkCompatible returned %v at the same head (block %d, time %d)",
				iterated, err, head, headTime)
		}
		if iterated != nil {
			checkCompatRewindBounds(t, iterated, head, headTime)
		}

		// Property 5: CheckConfigForkOrder never panics, and what it accepts is
		// ordered. A non-Parlia config is exempt from the second half: the
		// function returns nil before looking at anything (IsNotInBSC), so
		// "accepted" carries no claim there. The call is still made, because
		// not panicking is the other half of the property.
		for _, c := range []*ChainConfig{stored, moved} {
			orderErr := c.CheckConfigForkOrder()
			if orderErr == nil && c.IsInBSC() {
				checkCompatForkOrderAccepted(t, c, head, headTime)
			}
		}
	})
}

// compatWhatIn reports whether a ConfigCompatError.What is one of the What
// strings the moved forks entitle checkCompatible to report.
func compatWhatIn(what string, wantWhat []string) bool {
	for _, w := range wantWhat {
		if what == w {
			return true
		}
	}
	return false
}

// checkCompatRewindBounds asserts property 4 on one error.
func checkCompatRewindBounds(t *testing.T, err *ConfigCompatError, head, headTime uint64) {
	t.Helper()
	if err.RewindToBlock > head {
		t.Fatalf("%s: rewind to block %d is above the head block %d (stored %v, new %v)",
			err.What, err.RewindToBlock, head, err.StoredBlock, err.NewBlock)
	}
	if err.RewindToTime > headTime {
		t.Fatalf("%s: rewind to timestamp %d is above the head timestamp %d (stored %v, new %v)",
			err.What, err.RewindToTime, headTime, derefTime(err.StoredTime), derefTime(err.NewTime))
	}
}

// ---------------------------------------------------------------------------
// Companions
// ---------------------------------------------------------------------------

// TestChilizForksNotYetComparedIsCurrent is the deterministic half of the
// expected-to-fail bookkeeping: it does not depend on the fuzz corpus reaching
// a particular input. For each Chiliz fork field still on the list it moves
// that fork alone, with the head far past every schedule, and asserts that
// checkCompatible says "compatible" — the COR-218 gap, pinned.
//
// When COR-218 lands this test fails, once per fixed field, with the exact
// edit to make. When the list is empty it fails too, telling you to delete it.
func TestChilizForksNotYetComparedIsCurrent(t *testing.T) {
	pending := chilizForksNotYetCompared()
	if len(pending) == 0 {
		t.Fatalf("every Chiliz fork field is now compared by checkCompatible: COR-218 has landed. " +
			"Delete chilizForksNotYetCompared, TestChilizForksNotYetComparedIsCurrent, and the expected-to-fail " +
			"branch in FuzzCheckCompatible; FuzzCheckCompatible's property 2 then covers all eleven fields.")
	}
	head := new(big.Int).SetUint64(math.MaxUint64)
	for _, field := range pending {
		i := compatForkIndexOf(field)
		for _, sel := range []uint8{compatSelNil, compatSelValue(0), compatSelValue(1)} {
			stored := compatBuild(1, nil, true, math.MaxUint64, math.MaxUint64)
			moved := compatBuild(1, nil, true, math.MaxUint64, math.MaxUint64)
			compatApplySel(moved, i, sel, math.MaxUint64, math.MaxUint64)

			oldValue, oldSet := compatForkRead(stored, i)
			newValue, newSet := compatForkRead(moved, i)
			if oldSet == newSet && (!oldSet || oldValue == newValue) {
				continue // this selector is not a move for this field
			}
			if err := stored.checkCompatible(moved, head, math.MaxUint64); err != nil {
				t.Fatalf("COR-218 has landed for %s: checkCompatible now reports %q. Fill in its `what` string in "+
					"compatForkTable and drop its cor218 flag, so FuzzCheckCompatible's property 2 covers it.",
					field, err.What)
			}
		}
	}
	t.Logf("COR-218 still open: checkCompatible ignores %d Chiliz fork field(s): %v", len(pending), pending)
}

// TestCompatForkTableCoversChainConfig asserts that compatForkTable names every
// fork field of ChainConfig — every *big.Int or *uint64 field whose name ends
// in Block or Time. An upstream merge that adds a fork fails here until someone
// records whether checkCompatible defends it, which is exactly the decision
// COR-218 found nobody had made for the Chiliz forks.
func TestCompatForkTableCoversChainConfig(t *testing.T) {
	covered := make(map[string]bool, len(compatForks))
	for _, f := range compatForks {
		if covered[f.field] {
			t.Errorf("compatForkTable lists %s twice", f.field)
		}
		covered[f.field] = true
	}
	typ := reflect.TypeOf(ChainConfig{})
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if sf.Type != reflect.TypeOf((*big.Int)(nil)) && sf.Type != reflect.TypeOf((*uint64)(nil)) {
			continue
		}
		name := sf.Name
		if !strings.HasSuffix(name, "Block") && !strings.HasSuffix(name, "Time") {
			continue
		}
		if !covered[name] {
			t.Errorf("ChainConfig.%s looks like a fork field but is not in compatForkTable: add it, with the "+
				"ConfigCompatError.What string checkCompatible reports for it, or an empty What if checkCompatible "+
				"does not compare it (and a comment saying why that is acceptable)", name)
		}
	}
}
