# Builds the companion without Gradle. The SDK layout this expects is the one
# already on the laptop: platforms/android-35 and build-tools/35.0.0 under
# ~/Android. Override SDK and BUILD_TOOLS if yours differs.

SDK ?= $(HOME)/Android
PLATFORM ?= $(SDK)/platforms/android-35/android.jar
BUILD_TOOLS ?= $(SDK)/build-tools/35.0.0
AAPT2 := $(BUILD_TOOLS)/aapt2
D8 := $(BUILD_TOOLS)/d8
ZIPALIGN := $(BUILD_TOOLS)/zipalign
APKSIGNER := $(BUILD_TOOLS)/apksigner

PKG := dev.breadway.breadpair
APP := breadpair-companion.apk
KEYSTORE := $(HOME)/.local/share/breadpair/debug.keystore

SRC := $(shell find src -name '*.java' | sort)
RES := $(shell find res -type f | sort)

.PHONY: all clean install

all: $(APP)

gen/dev/breadway/breadpair/R.java: AndroidManifest.xml $(RES)
	mkdir -p gen
	$(AAPT2) compile --dir res -o compiled.zip
	$(AAPT2) link -I $(PLATFORM) --manifest AndroidManifest.xml \
		--java gen -o linked.apk --min-sdk-version 30 --target-sdk-version 35 \
		compiled.zip
	rm -f compiled.zip

classes: gen/dev/breadway/breadpair/R.java $(SRC)
	mkdir -p classes
	javac --release 17 -Xlint:-deprecation -classpath $(PLATFORM) -d classes \
		$(SRC) gen/dev/breadway/breadpair/R.java

dex/classes.dex: classes
	mkdir -p dex
	find classes -name '*.class' > classes.list
	$(D8) --min-api 30 --output dex @classes.list
	touch dex/classes.dex

dex: dex/classes.dex

unsigned.apk: dex/classes.dex gen/dev/breadway/breadpair/R.java $(RES) AndroidManifest.xml
	$(AAPT2) compile --dir res -o compiled.zip
	$(AAPT2) link -I $(PLATFORM) --manifest AndroidManifest.xml \
		-o unsigned.apk --min-sdk-version 30 --target-sdk-version 35 compiled.zip
	cd dex && zip -q -j ../unsigned.apk classes.dex
	rm -f compiled.zip

aligned.apk: unsigned.apk
	$(ZIPALIGN) -f 4 unsigned.apk aligned.apk

$(APP): aligned.apk $(KEYSTORE)
	$(APKSIGNER) sign --ks $(KEYSTORE) --ks-pass pass:breadpair --key-pass pass:breadpair \
		--out $(APP) aligned.apk
	$(APKSIGNER) verify $(APP)

$(KEYSTORE):
	mkdir -p $(dir $(KEYSTORE))
	keytool -genkeypair -keystore $(KEYSTORE) -storepass breadpair -keypass breadpair \
		-alias breadpair -keyalg RSA -keysize 2048 -validity 10000 \
		-dname 'CN=breadpair,O=breadway,C=AU'

install: $(APP)
	adb install -r $(APP)

clean:
	rm -rf gen classes dex compiled.zip linked.apk unsigned.apk aligned.apk $(APP)
