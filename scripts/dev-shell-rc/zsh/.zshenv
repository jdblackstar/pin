export ZDOTDIR="$PIN_DEV_ORIGINAL_ZDOTDIR"
if [[ -r "$ZDOTDIR/.zshenv" ]]; then
	source "$ZDOTDIR/.zshenv"
fi

# The original .zshenv may choose a different configuration directory. Keep
# that choice for the user's .zshrc and restore the wrapper just for startup.
export PIN_DEV_EFFECTIVE_ZDOTDIR=${ZDOTDIR:-$PIN_DEV_ORIGINAL_ZDOTDIR}
export ZDOTDIR="$PIN_DEV_WRAPPER_ZDOTDIR"
