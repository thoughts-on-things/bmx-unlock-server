BINARY   = bmx-unlock-server
HA_HOST  = hassio@homeassistant.local
HA_DIR   = /config/bmx-unlock-server

build:
	GOOS=linux GOARCH=arm64 GOEXPERIMENT=jsonv2 go build -o $(BINARY) .

# scp to a temp name first: overwriting a running binary fails with
# "text file busy". The webhook has HA restart the server (it must run
# inside the HA container) and re-push the dashboard door settings.
deploy: build
	scp -O $(BINARY) $(HA_HOST):$(HA_DIR)/$(BINARY).new
	ssh $(HA_HOST) "sudo docker exec homeassistant kill \$$(sudo docker exec homeassistant pgrep -f '/$(BINARY)$$') 2>/dev/null; sudo docker exec homeassistant mv $(HA_DIR)/$(BINARY).new $(HA_DIR)/$(BINARY)"
	curl -s -X POST http://homeassistant.local:8123/api/webhook/bmx-deploy
	rm $(BINARY)

logs:
	ssh $(HA_HOST) "tail -f $(HA_DIR)/server.log"
