package server

// postLanguages are offered in the web compose form, by their own names.
// Clients of the API can send any BCP 47 tag (see normLanguage).
var postLanguages = []struct{ Code, Name string }{
	{"ar", "العربية"}, {"bg", "Български"}, {"ca", "Català"}, {"cs", "Čeština"}, {"cy", "Cymraeg"},
	{"da", "Dansk"}, {"de", "Deutsch"}, {"el", "Ελληνικά"}, {"en", "English"}, {"eo", "Esperanto"},
	{"es", "Español"}, {"et", "Eesti"}, {"eu", "Euskara"}, {"fa", "فارسی"}, {"fi", "Suomi"},
	{"fr", "Français"}, {"ga", "Gaeilge"}, {"gl", "Galego"}, {"he", "עברית"}, {"hi", "हिन्दी"},
	{"hr", "Hrvatski"}, {"hu", "Magyar"}, {"id", "Bahasa Indonesia"}, {"is", "Íslenska"}, {"it", "Italiano"},
	{"ja", "日本語"}, {"ko", "한국어"}, {"lt", "Lietuvių"}, {"lv", "Latviešu"}, {"nb", "Norsk bokmål"},
	{"nl", "Nederlands"}, {"pl", "Polski"}, {"pt", "Português"}, {"ro", "Română"}, {"ru", "Русский"},
	{"sk", "Slovenčina"}, {"sl", "Slovenščina"}, {"sr", "Српски"}, {"sv", "Svenska"}, {"th", "ไทย"},
	{"tr", "Türkçe"}, {"uk", "Українська"}, {"vi", "Tiếng Việt"}, {"zh", "中文"},
}

// postLanguage is the language a new post starts with: the one of the
// account's latest post that had one ("" leaves it unset).
func (app *App) postLanguage(me *Account) string {
	if me == nil {
		return ""
	}
	var lang string
	app.db.QueryRow(`SELECT language FROM statuses WHERE account_id=? AND language != '' AND boost_of_id='' ORDER BY id DESC LIMIT 1`, me.ID).Scan(&lang)
	return lang
}
