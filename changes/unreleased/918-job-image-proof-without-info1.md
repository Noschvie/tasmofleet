---
section: Fixed
---
An update job through minimal or safeboot reads the device's main page again when no INFO1 names the image (an HTTP-only device, MQTT down) and always when the job runs over HTTP: the image at the address is the only proof during the job that another chip has not taken the address while the device restarted, and an image of another chip family stops the job. A job over MQTT with an INFO1 still needs no page (#918)
