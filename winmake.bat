setlocal
set pwd=%CD:~3%
set pwd=%pwd:\=/%
docker run --rm -v /host_mnt/%CD:~0,1%/%pwd%:/source -w /source golang:1.16.5 make build