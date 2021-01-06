# Rclone for Shopware

## Setting up

* Download the latest binary or compile from source
* Having a Shopware 6 Instance with APIv3 (min Shopware 6.3.0.0)
* Create in Shopware 6 -> Settings -> Integration a new Integration
    * Allow write permissions
* Run `rclone config` and create a new remote with filesystem `shopware` and fill the shop url and the created credentials


## Examples

### Serving a server

```bash
rclone serve http myRemoteName:
rclone serve sftp myRemoteName:
rclone serve webdav myRemoteName:
```

### Mounting it as filesystem

```bash
rclone mount myRemoteName: [path]
```

### Some basic commands

```bash
rclone copy local-file.txt myRemoteName:remote-file.txt
rclone cat myRemoteName:remote-file.txt
```

### Limitations

* You can upload only files that matches the php max_upload_size of the Shop
* The file name must be unique. Shopware 6 wants a unique name for all files
* Only upload works for allowed extensions of the Media Manager
