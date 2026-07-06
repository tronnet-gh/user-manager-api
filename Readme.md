# ProxmoxAAS Fabric - In-Memory Cache for ProxmoxAAS Cluster State
ProxmoxAAS Fabric provides functionality for the API by caching an in-memory representation of the proxmox cluster's state which speeds up computation of resource usage statistics.

## Installation

### Prerequisites
- [ProxmoxAAS-API](https://git.tronnet.net/tronnet/ProxmoxAAS-API)
- Proxmox VE Cluster (v7.0+)

### Installation - API
1. Download `proxmoxaas-fabric` binary and `template.config.json` file from releases
2. Rename `template.config.json` to `config.json` and modify the following values:
    1. `listenPort`: port for proxmoxaas-fabric to bind and listen on
    2. `url`: url to the Proxmox API, ie  `https://pve.domain.net/api2/json`
    3. `token`: the user(name), authentication realm (pam), token id, and token secrey key (uuid)
    4. `rebuildInterval` (***optional***): the interval on which to completely rebuild the cache
