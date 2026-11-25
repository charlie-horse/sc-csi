# Array setup

What the Storage Center needs before the driver can use it. The `hack/`
tools named here read the array address and an admin login from `.env`
(template: `.env.example`).

## REST access

- SCOS 7.x serves the REST API itself, on TCP 3033. A DSM Data Collector
  (Dell's management server) serves the same API; when it manages several
  Storage Centers, give the driver the serial number of the one to use.
- Check access: `curl -k https://<array>:3033/api/rest/` answers 401.

## TLS certificate pinning

Storage Centers ship self-signed certificates, often issued for an old
address, so normal hostname checks fail. The driver instead checks the
certificate's SHA-256 fingerprint ("pinning"). Get it with:

```bash
openssl s_client -connect <array>:3033 </dev/null | openssl x509 -noout -fingerprint -sha256
```

Each controller may present its own certificate. Give every fingerprint,
comma separated. The controller refuses to start without a fingerprint, a
CA file or an explicit opt-out of TLS checks; the setting names are in the
usage text of `cmd/sc-csi/main.go`.

## Account

- On SCOS 7.2 with direct REST access, only Admin-privilege accounts can
  log in; Volume Manager accounts get HTTP 401. Privileges can be raised
  but never lowered.
- Use a dedicated account, not the built-in Admin. It shows up separately
  in the array's audit log and its password can be changed on its own.
  `python3 -I hack/sc-maint.py csp-user --user <name> --folder <folder>`
  creates the user and a user group holding the driver's folders.
- The REST calls for this are not in Dell's public cookbook:
  - `ScUser` POST ignores `Password`; set it with `ScUser/{id}/ResetPassword`.
  - `ScUserGroup` POST ignores folder lists; add them with
    `ScUserGroup/{id}/AddVolumeFolder`, `AddServerFolder`, `AddDiskFolder`
    and `AddUserToGroup`.
- Behind a DSM Data Collector, a Volume Manager account limited to the user
  group works.

## Folders and profiles

- The driver creates its volume folder and server folder on first use, and
  only touches objects inside them.
- New volumes get the array's default snapshot schedule and data
  reduction. The driver replaces the schedule with the StorageClass's
  `snapshotProfile` (none when it is empty) and applies the storage and
  data-reduction profiles. See the array's defaults with
  `python3 -I hack/sc-probe.py info`.
- **A tiered storage profile needs a snapshot schedule.** With a profile
  that writes to a fast tier and keeps snapshot pages on a slower one,
  pages only move down once a replay has frozen them. Give such a
  StorageClass a `snapshotProfile` with a short expiry (for example Dell's
  `Daily_Best_Practices_System_Default`). Without one, all data stays on
  the fast tier.

## Fibre Channel zoning

- Zone every node FC port to the array's front-end ports on both fabrics.
  In virtual port mode, zone to the virtual WWPNs.
- `hack/sc-node.sh inventory`, run on a node, lists its FC ports, the array
  ports it sees and its multipath maps.

## Ports owned by other server objects

- If a node's FC port already belongs to a server object outside the
  driver's server folder (for example an old ESXi host), the driver skips
  that port and never changes the other object. The node is refused only
  when none of its ports is usable.
- To give such a port to the driver, detach it from the other object:
  `python3 -I hack/sc-maint.py remove-hbas --match '<server>'` (dry run),
  then add `--yes`.
- Volumes still mapped to the old object stay visible on the node. Unmap
  them, or keep the node's multipath configuration from claiming them
  ([node-requirements.md](node-requirements.md)).

### Server objects held by vVol bindings

If vCenter went away while vVols (VMware virtual volumes) were still bound,
the array keeps those bindings on each ESXi server object. The public REST
API cannot remove the vVols, their protocol-endpoint mappings, the server,
or its last FC port, and cannot change its OS type. Only the VASA provider
in a DSM Data Collector, or Dell support, can release the bindings.

What does work is to **adopt** the server objects: move them into the
driver's server folder, where the driver reuses them by WWPN.

```bash
python3 -I hack/sc-maint.py adopt-servers --match '<esxi-name-regex>' --folder <server-folder>        # dry run
python3 -I hack/sc-maint.py adopt-servers --match '<esxi-name-regex>' --folder <server-folder> --yes
```

- The leftover protocol endpoint stays mapped. Keep it out of multipath
  with the WWID exception in [node-requirements.md](node-requirements.md).
- If the node already has its own server object in the folder, its ports
  now belong to two objects and mapping fails with "WWPNs of <node> belong
  to several SC servers". Delete the node's own object (it has no volumes):
  `python3 -I hack/sc-maint.py wipe-servers --match '^<server>$'` (dry
  run), then `--yes`.
- Verify I/O and multipath on the first adopted node.
