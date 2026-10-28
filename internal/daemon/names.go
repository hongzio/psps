package daemon

import (
	"errors"
	"math/rand/v2"
	"strconv"

	"github.com/hongzio/gero/internal/store"
)

// adjectives and nouns make what an agent given no ID of its own goes by,
// adjective-noun: 250,000 of them. Neither list has a word that reads
// badly next to any of the other's: no people, nothing crude.
var adjectives = []string{
	"amber", "aqua", "azure", "beige", "blue", "bronze", "brown", "cerise", "coral",
	"crimson", "cyan", "emerald", "golden", "gray", "green", "indigo", "ivory", "jade",
	"khaki", "lavender", "lemon", "lilac", "lime", "magenta", "maroon", "mauve", "mint",
	"navy", "ochre", "olive", "orange", "peach", "pearl", "pink", "plum", "purple", "rose",
	"ruby", "rust", "saffron", "sage", "sapphire", "scarlet", "sepia", "silver", "tan",
	"teal", "topaz", "umber", "violet", "copper", "cobalt", "cherry", "honey", "hazel",
	"russet", "bright", "brilliant", "clear", "cloudy", "crisp", "dappled", "dazzling",
	"gleaming", "glinting", "glossy", "glowing", "hazy", "luminous", "lustrous", "matte",
	"misty", "polished", "radiant", "shadowy", "shimmering", "shiny", "sleek", "smoky",
	"sparkling", "speckled", "spotted", "starry", "striped", "sunlit", "translucent",
	"twinkling", "vivid", "frosted", "gilded", "painted", "dotted", "marbled", "mottled",
	"glassy", "silky", "velvet", "woven", "pastel", "neon", "big", "broad", "compact",
	"curved", "curly", "deep", "giant", "grand", "huge", "large", "little", "long", "mini",
	"narrow", "petite", "round", "short", "slim", "small", "square", "steep", "tall",
	"teeny", "tiny", "vast", "wide", "wavy", "twisty", "spiral", "oval", "angular", "flat",
	"lofty", "mighty", "massive", "slender", "stout", "jumbo", "bouncy", "bumpy", "crunchy",
	"downy", "feathery", "fluffy", "fuzzy", "grainy", "leafy", "mossy", "plush", "prickly",
	"rocky", "rubbery", "sandy", "smooth", "soft", "solid", "spongy", "springy", "sturdy",
	"velvety", "wooden", "woolly", "wiry", "rugged", "stony", "metallic", "crystal", "balmy",
	"breezy", "chilly", "cool", "cozy", "dewy", "drizzly", "early", "foggy", "frosty",
	"mild", "rainy", "snowy", "stormy", "sunny", "toasty", "tropical", "warm", "windy",
	"wintry", "autumnal", "summery", "vernal", "nocturnal", "lunar", "solar", "polar",
	"arctic", "alpine", "coastal", "oceanic", "rustic", "urban", "rural", "cosmic",
	"stellar", "celestial", "astral", "galactic", "orbital", "atomic", "electric",
	"magnetic", "sonic", "digital", "analog", "binary", "quantum", "able", "adept", "agile",
	"alert", "amiable", "amused", "ardent", "artful", "astute", "avid", "awake", "aware",
	"bashful", "blithe", "bold", "brainy", "brave", "bubbly", "busy", "calm", "candid",
	"careful", "caring", "charming", "cheeky", "cheerful", "cheery", "chipper", "chirpy",
	"civil", "classy", "clever", "comfy", "cordial", "courtly", "crafty", "curious",
	"daring", "dashing", "dauntless", "dear", "decent", "deft", "devoted", "diligent",
	"dreamy", "droll", "dutiful", "eager", "earnest", "elated", "elegant", "eloquent",
	"eminent", "epic", "exact", "fabled", "fair", "faithful", "famous", "fancy", "fearless",
	"feisty", "festive", "fiery", "fine", "fleet", "fond", "frank", "free", "fresh",
	"friendly", "frugal", "funny", "gallant", "gentle", "genial", "gifted", "giddy", "glad",
	"gleeful", "global", "glorious", "good", "graceful", "gracious", "grateful", "gutsy",
	"handy", "happy", "hardy", "hearty", "helpful", "heroic", "honest", "hopeful", "humble",
	"jaunty", "jolly", "jovial", "joyful", "joyous", "jubilant", "keen", "kind", "kindly",
	"knowing", "lively", "logical", "loyal", "lucid", "lucky", "merry", "mellow", "mindful",
	"modest", "neat", "nifty", "nimble", "noble", "open", "patient", "peppy", "placid",
	"playful", "plucky", "poised", "polite", "proud", "prudent", "quick", "quiet", "quirky",
	"rapid", "ready", "regal", "relaxed", "reliable", "resolute", "robust", "rosy", "royal",
	"safe", "savvy", "serene", "sharp", "shy", "silly", "sincere", "skilled", "sleepy",
	"smart", "smiling", "snappy", "snug", "sociable", "spirited", "splendid", "sporty",
	"spry", "stable", "stately", "steady", "stoic", "super", "swift", "tactful", "tender",
	"thrifty", "tidy", "tireless", "tranquil", "true", "trusty", "upbeat", "valiant",
	"verdant", "vibrant", "vigilant", "vigorous", "wise", "witty", "wondrous", "worthy",
	"zany", "zealous", "zesty", "zen", "ancient", "antique", "classic", "distant", "eternal",
	"floral", "fragrant", "hidden", "idle", "infinite", "lone", "loud", "magic", "magical",
	"major", "modern", "mystic", "new", "northern", "novel", "organic", "plain", "primal",
	"prime", "pure", "rare", "rich", "roving", "secret", "silent", "simple", "solo",
	"southern", "still", "sublime", "subtle", "sweet", "tangy", "tidal", "timely", "vintage",
	"wandering", "western", "eastern", "wild", "zippy", "buttery", "crispy", "honeyed",
	"minty", "nutty", "peppery", "savory", "sugary", "citrus", "fizzy", "humming", "chiming",
	"melodic", "musical", "rhythmic", "sonorous", "whistling", "buzzing", "purring",
	"singing", "dancing", "drifting", "flying", "gliding", "jumping", "leaping", "roaming",
	"rolling", "running", "sailing", "skipping", "soaring", "spinning", "swaying",
	"swimming", "swirling", "twirling", "whirling", "blooming", "budding", "bustling",
	"sprouting", "floating", "flowing", "rising", "shining", "beaming", "blazing",
	"flickering", "wobbly", "active",
}

var nouns = []string{
	"aardvark", "albatross", "alpaca", "anchovy", "anteater", "antelope", "armadillo",
	"badger", "barracuda", "bat", "bear", "beetle", "bison", "bluebird", "bobcat", "buffalo",
	"bumblebee", "butterfly", "camel", "canary", "capybara", "cardinal", "caribou", "cat",
	"caterpillar", "catfish", "chameleon", "cheetah", "chickadee", "chinchilla", "chipmunk",
	"cicada", "clam", "cobra", "condor", "cormorant", "coyote", "crab", "crane", "cricket",
	"crow", "cuckoo", "dingo", "dolphin", "dove", "dragonfly", "duck", "eagle", "eel",
	"egret", "elephant", "elk", "emu", "falcon", "ferret", "finch", "firefly", "flamingo",
	"fox", "frog", "gazelle", "gecko", "gerbil", "giraffe", "gnu", "goat", "goldfinch",
	"goose", "grasshopper", "grouse", "guppy", "hamster", "hare", "hawk", "hedgehog",
	"heron", "herring", "hippo", "hornet", "horse", "hummingbird", "hyena", "ibex", "ibis",
	"iguana", "impala", "jackal", "jaguar", "jay", "jellyfish", "kangaroo", "kestrel",
	"kingfisher", "koala", "koi", "ladybug", "lark", "lemur", "leopard", "lion", "llama",
	"lobster", "lynx", "macaw", "magpie", "mallard", "manatee", "mantis", "marlin", "marmot",
	"meerkat", "mink", "mole", "moose", "moth", "narwhal", "newt", "nightingale", "ocelot",
	"octopus", "okapi", "opossum", "orca", "oriole", "osprey", "ostrich", "otter", "owl",
	"ox", "oyster", "panda", "panther", "parrot", "partridge", "peacock", "pelican",
	"penguin", "pheasant", "pigeon", "platypus", "plover", "pony", "porcupine", "porpoise",
	"puffin", "puma", "quail", "rabbit", "raven", "reindeer", "robin", "salamander",
	"salmon", "sandpiper", "sardine", "scorpion", "seahorse", "seal", "shark", "sheep",
	"shrimp", "sloth", "snail", "sparrow", "squid", "squirrel", "starfish", "stingray",
	"stork", "swan", "tapir", "tarsier", "termite", "tern", "tiger", "toad", "tortoise",
	"toucan", "trout", "tuna", "turtle", "urchin", "viper", "vole", "walrus", "warbler",
	"whale", "wildcat", "wolf", "wombat", "woodpecker", "wren", "yak", "zebra", "acorn",
	"almond", "aloe", "apple", "apricot", "ash", "aspen", "bamboo", "basil", "bay", "beech",
	"berry", "birch", "blossom", "bluebell", "bramble", "buttercup", "cactus", "camellia",
	"cedar", "chestnut", "clover", "coconut", "cypress", "daffodil", "dahlia", "daisy",
	"dandelion", "elm", "fern", "fig", "fir", "foxglove", "gardenia", "geranium", "heather",
	"hibiscus", "holly", "iris", "ivy", "jasmine", "juniper", "kale", "laurel", "lily",
	"linden", "lotus", "magnolia", "maple", "marigold", "moss", "mulberry", "myrtle",
	"nutmeg", "oak", "orchid", "palm", "papaya", "pear", "peony", "pepper", "petunia",
	"pine", "poppy", "primrose", "pumpkin", "quince", "redwood", "rosemary", "rowan",
	"sequoia", "sorrel", "spruce", "sunflower", "sycamore", "tamarind", "thistle", "thyme",
	"tulip", "vanilla", "walnut", "willow", "wisteria", "yew", "zinnia", "beach", "brook",
	"canyon", "cape", "cave", "cliff", "cloud", "coast", "comet", "cove", "crater", "creek",
	"dale", "delta", "desert", "dune", "dusk", "ember", "estuary", "field", "fjord",
	"forest", "frost", "galaxy", "geyser", "glacier", "glade", "glen", "grove", "gulf",
	"harbor", "heath", "hill", "horizon", "island", "isle", "jungle", "lagoon", "lake",
	"marsh", "meadow", "mesa", "mist", "moon", "moor", "mountain", "nebula", "oasis",
	"ocean", "orbit", "peak", "pebble", "planet", "pond", "prairie", "quasar", "rain",
	"rainbow", "reef", "ridge", "river", "savanna", "sea", "shore", "sky", "spring", "star",
	"storm", "stream", "summit", "sun", "thunder", "tide", "tundra", "valley", "volcano",
	"wave", "wind", "anvil", "arrow", "badge", "balloon", "banjo", "barrel", "basket",
	"beacon", "bell", "bench", "blanket", "boat", "bottle", "bucket", "button", "cabin",
	"camera", "candle", "canoe", "canvas", "carpet", "castle", "chalk", "chime", "clock",
	"compass", "crayon", "crown", "cup", "cushion", "dial", "drum", "easel", "engine",
	"fiddle", "flute", "fountain", "gadget", "garden", "gate", "glove", "guitar", "hammer",
	"harp", "hat", "hive", "horn", "kettle", "key", "kite", "ladder", "lamp", "lantern",
	"lens", "locket", "loom", "magnet", "mandolin", "map", "marble", "mirror", "mitten",
	"mosaic", "notebook", "oar", "paddle", "pencil", "piano", "pillow", "pocket", "puzzle",
	"quill", "quilt", "radio", "raft", "ribbon", "rocket", "rudder", "saddle", "sail",
	"satchel", "scarf", "scroll", "shovel", "sled", "spoon", "stamp", "statue", "teapot",
	"telescope", "tent", "thimble", "ticket", "torch", "tower", "trumpet", "tuba",
	"umbrella", "vase", "violin", "wagon", "wheel", "whistle", "windmill", "yarn", "yoyo",
	"zipper", "amethyst", "agate", "basalt", "diamond", "garnet", "granite", "jasper",
	"obsidian", "onyx", "opal", "quartz", "bagel", "biscuit", "brownie", "butter", "caramel",
	"cashew", "cheese", "cider", "cinnamon", "cocoa", "coffee", "cupcake", "custard",
	"donut", "dumpling", "espresso", "fudge", "granola", "jelly", "lemonade", "macaron",
	"mango", "marzipan", "noodle", "nougat",
}

// newAgentHandle is an adjective-noun no agent goes by, as its own ID or
// as its key; should a hundred picks all be taken, one with a number.
func (m *Manager) newAgentHandle() (string, error) {
	all, err := m.store.Agents()
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, a := range all {
		taken[a.ID], taken[a.Handle] = true, true
	}
	pick := func() string { return adjectives[rand.IntN(len(adjectives))] + "-" + nouns[rand.IntN(len(nouns))] }
	for range 100 {
		if n := pick(); !taken[n] {
			return n, nil
		}
	}
	for i := 2; ; i++ {
		if n := pick() + "-" + strconv.Itoa(i); !taken[n] {
			return n, nil
		}
	}
}

// storeNamed stores a new agent, picking its name again if another agent
// took that one meanwhile; an ID the operator chose is refused instead.
func (m *Manager) storeNamed(a store.Agent, picked bool) (store.Agent, error) {
	for try := 0; ; try++ {
		err := m.store.CreateAgent(a)
		if !picked || try == 4 || !errors.Is(err, store.ErrExists) {
			return a, err
		}
		if a.Handle, err = m.newAgentHandle(); err != nil {
			return a, err
		}
	}
}
