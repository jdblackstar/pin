export ZDOTDIR=${PIN_DEV_EFFECTIVE_ZDOTDIR:-$PIN_DEV_ORIGINAL_ZDOTDIR}
if [[ -r "$ZDOTDIR/.zshrc" ]]; then
	source "$ZDOTDIR/.zshrc"
fi

# Do not let startup configuration derived from the temporary wrapper path put
# shell history in the repository.
if [[ -n ${HISTFILE-} && $HISTFILE == "$PIN_DEV_WRAPPER_ZDOTDIR"/* ]]; then
	HISTFILE="${PIN_DEV_EFFECTIVE_ZDOTDIR:-$PIN_DEV_ORIGINAL_ZDOTDIR}/${HISTFILE:t}"
fi

if [[ -z ${PROMPT-} ]]; then
	PROMPT='%# '
fi
PROMPT="[pin dev:${PIN_DEV_PROFILE}] $PROMPT"
PS1=$PROMPT
