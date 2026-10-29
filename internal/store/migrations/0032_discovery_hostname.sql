-- 0032_discovery_hostname: the hostname a discovered device reported (discovery config "hn",
-- Status 5 or a scan), shown in the New devices list (#130). Empty: not reported (rows from
-- before this migration). It is display only and not part of a proposal's version.
ALTER TABLE discovery_proposals ADD COLUMN hostname TEXT NOT NULL DEFAULT '';
