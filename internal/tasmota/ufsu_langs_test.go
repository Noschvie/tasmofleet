package tasmota

// ufsuLanguage holds the translated words of one language build (tasmota/language/<file>) that the
// /ufsu result page contains (HandleUploadUFSDone, xdrv_50_filesystem.ino) plus D_UPLOAD_ERR_2, a
// failure reason the upload handler can name.
type ufsuLanguage struct {
	file, lang, upload, successful, failed, reason, manageFS string
}

// ufsuLanguages is generated from tasmota/language/*.h of the Tasmota development branch (a281cec,
// 2026-11-05): all 28 language files. Add the new rows when upstream adds a language.
var ufsuLanguages = []ufsuLanguage{
	{"af_AF.h", "af", "Laai op", "Suksesvol", "Misluk\t", "Nie genoeg plek nie", "Bestuur lêerstelsel"},
	{"bg_BG.h", "bg", "Качването е", "Успешно", "Неуспешно", "Недостатъчно свободно място", "Файлова система"},
	{"ca_AD.h", "ca", "Envia", "Resultat correcte", "Fallada", "No queda prou espai", "Gestionar el sistema d'arxius"},
	{"cs_CZ.h", "cs", "Nahrání...", "úspěšné.", "chyba", "Málo místa", "Manage File system"},
	{"de_DE.h", "de", "Upload", "erfolgreich", "fehlgeschlagen", "Nicht genug Speicherplatz", "Dateisystem verwalten"},
	{"el_GR.h", "el", "Ανέβασμα", "Επιτυχές", "Απέτυχε", "Δεν υπάρχει επαρκής χώρος", "Manage File system"},
	{"en_GB.h", "en", "Upload", "Successful", "Failed", "Not enough space", "Manage File system"},
	{"es_ES.h", "es", "Carga", "Exitosa", "Falló", "Espacio insuficiente", "Explorar Archivos"},
	{"fr_FR.h", "fr", "Upload", "Réussi", "Échoué", "Espace insuffisant", "Gestion du Système de Fichier"},
	{"fy_NL.h", "fy", "Stjoere", "Sukses", "Mislearre", "Net genôch romte", "Bestânbehearder"},
	{"he_HE.h", "he", "העלאה", "הצליח", "נכשל", "אין מספיק מקום", "Manage File system"},
	{"hu_HU.h", "hu", "Feltöltés", "Sikeres", "Sikertelen", "Nincs elég memória", "Fájlrendszer kezelése"},
	{"it_IT.h", "it", "Caricamento", "Completato", "Fallito", "Spazio insufficiente", "Gestione File system"},
	{"ko_KO.h", "ko", "업로드", "성공", "실패", "용량이 충분하지 않습니다", "Manage File system"},
	{"lt_LT.h", "lt", "Įkelti", "Sėkmingai", "Nepavyko", "Nepakanka vietos", "Valdyti failų sistemą"},
	{"nl_NL.h", "nl", "Verzenden", "Gelukt", "Mislukt", "Onvoldoende geheugen ruimte", "Bestandsbeheer"},
	{"pl_PL.h", "pl", "Wgraj", "Powodzenie", "Błędny", "Niewystarczająca ilość miejsca", "Menadżer plików"},
	{"pt_BR.h", "pt", "Enviar", "Successo", "Falha", "Não existe memória disponível", "Gerenciar sistema de arquivos"},
	{"pt_PT.h", "pt", "Enviar", "Successo", "Falha", "Não existe espaço disponível", "Gerenciar sistema de arquivos"},
	{"ro_RO.h", "ro", "Încărcăre", "Succes", "Eșuat", "Spațiu insuficient", "Manage File system"},
	{"ru_RU.h", "ru", "Загрузить", "Успешно", "Неудачно", "Недостаточно места", "Управление файловой системой"},
	{"sk_SK.h", "sk", "Nahrávanie...", "úspešné.", "chyba", "Málo miesta", "Manage File system"},
	{"sv_SE.h", "sv", "Ladda upp", "Lyckat", "Misslyckades", "Inte tillräckligt med ledigt utrymme", "Manage File system"},
	{"tr_TR.h", "tr", "Yükleme", "Başarıyla Tamamlandı", "Başlarısız", "Boş yer yok", "Manage File system"},
	{"uk_UA.h", "uk", "Завантажити", "Успішно", "Невдало", "Недостатньо місця", "Manage File system"},
	{"vi_VN.h", "vi", "Tải lên", "Thành công", "Thất bại", "Không đủ bộ nhớ", "Manage File system"},
	{"zh_CN.h", "zh", "上传", "成功", "失败", "没有足够空间", "Manage File system"},
	{"zh_TW.h", "zh-Hant-TW", "上傳", "已成功", "失敗", "可用空間不足", "管理檔案系統"},
}
