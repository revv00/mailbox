# provide path to a large file and use the multi config
export TEST_FILE_TO_WRITE=/home/plusai/Downloads/qoder_amd64.deb
export TEST_CONFIG=./pkg/object/mailfs_conf_multi.json
#go test -v ./pkg/object -run TestLargeFileWriteMultiAccounts
go test -v ./pkg/object -run TestJuiceFSBigFileWrite

#export TEST_CONFIG=./pkg/object/mailfs_conf_multi.json
#go test -v ./pkg/object -run TestJuiceFSForceIMAPRead