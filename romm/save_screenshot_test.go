package romm

import "testing"

func TestScreenshotName(t *testing.T) {
	cases := map[[2]string]string{
		{"/tmp/Game.sfc.sav", "/shots/SFC/Game.sfc.png"}:    "Game.sfc.png",
		{"/x/Game [2026-11-25_10-00-00].srm", "/y/pic.JPG"}: "Game [2026-11-25_10-00-00].jpg",
	}
	for in, want := range cases {
		if got := screenshotName(in[0], in[1]); got != want {
			t.Errorf("screenshotName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestSaveScreenshotPath(t *testing.T) {
	var none Save
	if got := none.ScreenshotPath(); got != "" {
		t.Errorf("a save with no picture gave %q", got)
	}

	var withPath Save
	withPath.Screenshot.ID = 4
	withPath.Screenshot.DownloadPath = "/api/raw/assets/users/1/screenshots/snes/3/Game.png?timestamp=1"
	if got := withPath.ScreenshotPath(); got != withPath.Screenshot.DownloadPath {
		t.Errorf("got %q, want the download path", got)
	}

	var onlyFull Save
	onlyFull.Screenshot.FileName = "Game.png"
	onlyFull.Screenshot.FullPath = "/users/1/screenshots/Game.png"
	if got := onlyFull.ScreenshotPath(); got != "/api/raw/assets/users/1/screenshots/Game.png" {
		t.Errorf("got %q", got)
	}
	if got := onlyFull.ScreenshotExt(); got != ".png" {
		t.Errorf("ext %q, want .png", got)
	}
}
