package repo

type Word struct {
	ID          int64  `json:"id"`
	Word        string `json:"word"`
	Phonetic    string `json:"phonetic"`
	Definition  string `json:"definition"`
	Translation string `json:"translation"`
	Pos         string `json:"pos"`
	Collins     int    `json:"collins"`
	Oxford      int    `json:"oxford"`
	Tag         string `json:"tag"`
	BNC         int    `json:"bnc"`
	FRQ         int    `json:"frq"`
	Exchange    string `json:"exchange"`
	Detail      string `json:"detail"`
	Audio       string `json:"audio"`
}
