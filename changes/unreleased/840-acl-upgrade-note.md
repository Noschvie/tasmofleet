---
section: Upgrade notes
---
Set up an ACL on your MQTT broker before you update: since this release MQTT is the first way TasmoFleet talks to a device that is online on its broker, so the broker delivers every command, every password in it and the whole configuration dump of every backed-up or restored device to every client that may subscribe to the device's topics (Home Assistant, Node-RED, MQTT Explorer, other devices), and without an ACL that is every client of the broker. docs/mqtt-broker-acl.md has a tested rule set. The Mosquitto app of Home Assistant does not enforce the `acl_file` of its documented recipe in version 7.1.3 (reported since 7.0.0, upstream app-mosquitto#5): with it, use another broker for the devices until it is fixed (#840)
