# nvm, as on boat.dev. --no-use: the default `node` stays /usr/local/bin/node;
# `nvm use 22` switches to the pre-installed Node 22 for this shell.
if [ -n "${BASH_VERSION:-}${ZSH_VERSION:-}" ] && [ -s /usr/local/nvm/nvm.sh ]; then
  export NVM_DIR=/usr/local/nvm
  . "$NVM_DIR/nvm.sh" --no-use
fi
