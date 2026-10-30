// excerpts of tasmota/tasmota_support/support_command.ino and support.ino
const char kTasmotaCommands[] PROGMEM = "|"  // No prefix
  // SetOptions synonyms
  D_SO_WIFINOSLEEP "|"
  // Other commands
  D_CMND_OTAURL "|"
#ifndef FIRMWARE_MINIMAL
  D_CMND_BACKLOG "|" D_CMND_POWER "|" D_CMND_STATUS "|" D_CMND_TELEPERIOD "|"
  D_CMND_SO "|" D_CMND_SETOPTION "|" D_CMND_MODULE "|" D_CMND_TEMPLATE "|" D_CMND_GPIO "|" D_CMND_FRIENDLYNAME
#endif  //FIRMWARE_MINIMAL
  ;

SO_SYNONYMS(kTasmotaSynonyms,
  127,
);

void (* const TasmotaCommand[])(void) PROGMEM = {
  &CmndOtaUrl,
#ifndef FIRMWARE_MINIMAL
  &CmndBacklog, &CmndPower, &CmndStatus, &CmndTeleperiod,
  &CmndSetoption, &CmndSetoption, &CmndModule, &CmndTemplate, &CmndGpio, &CmndFriendlyname
#endif  //FIRMWARE_MINIMAL
};

void ResponseCmndNumber(int value) {
  Response_P(S_JSON_COMMAND_NVALUE, XdrvMailbox.command, value);
}

void ResponseCmndChar(const char* value) {
  Response_P(S_JSON_COMMAND_SVALUE, XdrvMailbox.command, EscapeJSONString(value).c_str());
}

// ResponseCmndChar: the key is the command name as the table spells it
void CmndOtaUrl(void)
{
  if (XdrvMailbox.data_len > 0) {
    SettingsUpdateText(SET_OTAURL, XdrvMailbox.data);
  }
  ResponseCmndChar(SettingsText(SET_OTAURL));
}

// ResponseCmndNumber, reached through a differently spelled handler name
void CmndTeleperiod(void)
{
  if ((XdrvMailbox.payload >= 0) && (XdrvMailbox.payload < 3601)) {
    Settings->tele_period = XdrvMailbox.payload;
  }
  ResponseCmndNumber(Settings->tele_period);
}

// ResponseClear: the reply is published elsewhere (ExecuteCommandPower), so the keys are loose
void CmndPower(void)
{
  if ((XdrvMailbox.index > 0) && (XdrvMailbox.index <= TasmotaGlobal.devices_present)) {
    ExecuteCommandPower(XdrvMailbox.index, XdrvMailbox.payload, SRC_IGNORE);
    ResponseClear();
  }
  else if (0 == XdrvMailbox.index) {
    SetAllPower(XdrvMailbox.payload, SRC_IGNORE);
    ResponseClear();
  }
}

// ResponseClear when arguments are given, ResponseCmndChar without: still loose
void CmndBacklog(void) {
  if (XdrvMailbox.data_len) {
    ResponseClear();
  } else {
    ResponseCmndChar(blflag ? PSTR(D_JSON_EMPTY) : PSTR(D_JSON_ABORTED));
  }
}

// Status N goes out on stat/<topic>/STATUS<N>, not as a RESULT
void CmndStatus(void)
{
  Response_P(PSTR("{\"" D_CMND_STATUS "\":{\"Module\":%d}}"), Settings->module);
  MqttPublishPrefixTopic_P(option, PSTR(D_CMND_STATUS "1"));
}

// Response_P with a key that is not the command name: Template answers NAME, ARCH, GPIO, ...
void CmndTemplate(void)
{
  if (JsonTemplate(XdrvMailbox.data)) {
  } else {
    ResponseCmndChar(PSTR(D_JSON_INVALID_JSON));
  }
  TemplateJson();
}

void TemplateJson(void) {
  Response_P(PSTR("{\"" D_JSON_NAME "\":\"%s\",\"" D_JSON_ARCH "\":\"%s\",\"" D_JSON_GPIO "\":["),
    SettingsText(SET_TEMPLATE_NAME), ArchName().c_str());
  for (uint32_t i = 0; i < nitems(Settings->user_template.gp.io); i++) {
    ResponseAppend_P(PSTR("%s%d"), (i>0)?",":"", gpio);
  }
  ResponseAppend_P(PSTR("],\"" D_JSON_FLAG "\":%d,\"" D_JSON_BASE "\":%d}"), Settings->user_template.flag, Settings->user_template_base +1);
}

// a key with a number chosen at run time (GPIO0, GPIO1, ...) cannot be listed
void CmndGpio(void)
{
  Response_P(PSTR("{"));
  for (uint32_t i = 0; i < nitems(Settings->my_gp.io); i++) {
    ResponseAppend_P(PSTR("%s\"" D_CMND_GPIO "%d\":"), (jsflg) ? "," : "{", i);
  }
}

// ResponseCmndIdxChar: the key carries the index (SetOption19 -> SetOption19)
// usridx: without an index SetOption reports a list, with one a single option
void CmndSetoption(void) {
  if (!XdrvMailbox.usridx) {
    Response_P(PSTR("{"));
    SetOptionShow(0);
    ResponseAppend_P(PSTR("}"));
  } else {
    snprintf_P(XdrvMailbox.command, CMDSZ, PSTR(D_CMND_SETOPTION));  // Rename result shortcut command SO to SetOption
    CmndSetoptionBase(1);
  }
}

void SetOptionShow(uint32_t option) {
  ResponseAppend_P(PSTR("\"SetOptionSet\":["));
}

// indexed=1 answers SetOption16, indexed=0 (the synonyms, e.g. WifiNoSleep) answers the synonym name
void CmndSetoptionBase(bool indexed) {
  if (SetoptionDecode(XdrvMailbox.index, &ptype, &pindex)) {
    if (1 == ptype) {
      if (indexed) {
        ResponseCmndIdxNumber(Settings->param[pindex]);
      } else {
        ResponseCmndNumber(Settings->param[pindex]);
      }
    } else {
      if (indexed) {
        ResponseCmndIdxChar(GetStateText(bitRead(flag, pindex)));
      } else {
        ResponseCmndChar(GetStateText(bitRead(flag, pindex)));
      }
    }
  }
}

// ResponseCmndAll without an index, ResponseCmndIdxChar with one
void CmndFriendlyname(void) {
  if ((XdrvMailbox.index > 0) && (XdrvMailbox.index <= MAX_FRIENDLYNAMES)) {
    if (!XdrvMailbox.usridx) {
      ResponseCmndAll(SET_FRIENDLYNAME1, MAX_FRIENDLYNAMES);
    } else {
      ResponseCmndIdxChar(SettingsText(SET_FRIENDLYNAME1 + XdrvMailbox.index -1));
    }
  }
}

void ExecuteCommand(const char *cmnd, uint32_t source)
{
      if (!DecodeCommand(kTasmotaCommands, TasmotaCommand, kTasmotaSynonyms)) {
        if (!XdrvCall(FUNC_COMMAND)) {
        }
      }
}

// Module2 is the fallback module: the handler appends "2" to the command name at run time
void CmndModule(void)
{
  if (XdrvMailbox.index == 2) {
    strlcat(XdrvMailbox.command, "2", CMDSZ);
  }
  Response_P(S_JSON_COMMAND_NVALUE_SVALUE, XdrvMailbox.command, module_number, AnyModuleName(module_real).c_str());
}
