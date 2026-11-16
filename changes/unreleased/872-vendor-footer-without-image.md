---
section: Fixed
---
The update preview no longer refuses a device whose firmware footer leaves out the image name, as some vendor builds do ("Tasmota 14.3.0 von Theo Arends" plus a "Flashed by" line): the language build comes from the device's Status 4 locale. A build that also reports an unrecognised image in Status 2 (like tasmota-NOUS_A1T_DE) is still only replaced when you enter a Variant override (#872, thanks @rhasselbusch)
