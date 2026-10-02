FROM mcr.microsoft.com/playwright:v1.63.0-jammy

# The app's CSS asks for -apple-system/system-ui (Apple's SF Pro on a real
# device). SF Pro isn't redistributable via apt, so alias those names to
# Inter, a free, metrically close stand-in — see fontconfig-local.conf.
RUN apt-get update -qq \
  && apt-get install -y --no-install-recommends fonts-inter \
  && rm -rf /var/lib/apt/lists/*
COPY fontconfig-local.conf /etc/fonts/local.conf
RUN fc-cache -f

WORKDIR /app
RUN npm install --no-save playwright
COPY capture.js .

ENTRYPOINT ["node", "capture.js"]
