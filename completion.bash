# she in Git Bash, for people typing (Claude Code sends stdin and needs none
# of this). Read from ~/.bashrc: she --bash-completion > ~/.config/she/completion.bash
#
#   she [NAME] <  then Tab    she [NAME] <<<'|'           (| is the cursor)
#   Tab / Shift+Tab again     she [NAME] <<'E' / | / E    and back
#   inside the quotes of <<<  Tab completes from the server's bash (in NAME's
#                             directory), e.g. 'cd /ho -> 'cd /home/
#   Enter in the body of      on a line with text adds a line; on an empty
#     she <<'E' ... E         line (dropped) or on the E line runs it
#   she NAME alone            reads a line without showing it (a password)
#                             and sends it to the command running in NAME
#
# Any other Tab, Shift+Tab and Enter work as before.

she() {
  if (($# == 0)) && [[ -t 0 ]]; then
    command she -h
  elif (($# == 1)) && [[ $1 != -* && -t 0 ]]; then
    local __she_in
    IFS= read -rs -p "she: input for $1 (not shown): " __she_in
    echo >&2
    command she --input "$1" <<< "$__she_in"
  else
    command she "$@"
  fi
}

# the forms for the head "she [NAME] ", and where the cursor goes in each
__she_forms() {
  __she_f=("$1<<<''" "$1<<'E'"$'\n\n'"E")
  __she_p=($((${#1} + 4)) $((${#1} + 6)))
}

# Tab (1) and Shift+Tab (-1): make or switch the forms; anything else goes on
# to readline's own completion (\C-x\C-r is bound to what runs next)
__she_tab() {
  local l=$READLINE_LINE p=$READLINE_POINT h i j s next=complete
  local re='^([[:space:]]*she([[:space:]]+[^-[:space:]<>|;&][^[:space:]<>|;&]*)?[[:space:]]*)(<|<<|<<<)$'
  local rh='^[[:space:]]*she([[:space:]]+[^-[:space:]<>|;&][^[:space:]<>|;&]*)?[[:space:]]+$'
  local -a sfx=("<<<''" "<<'E'"$'\n\n'"E")
  if ((p == ${#l})) && [[ $l =~ $re ]]; then
    # she <  and  she <<<  make <<<'|';  she <<  makes <<'E' ... E
    h=${BASH_REMATCH[1]}
    i=0
    [[ ${BASH_REMATCH[3]} == "<<" ]] && i=1
    [[ $h == *[[:space:]] ]] || h+=" "
    __she_forms "$h"
    READLINE_LINE=${__she_f[i]} READLINE_POINT=${__she_p[i]}
    next=none
  else
    for i in 0 1; do
      s=${sfx[i]}
      [[ $l == *"$s" ]] || continue
      h=${l%"$s"}
      [[ $h =~ $rh ]] || break
      __she_forms "$h"
      if ((p == __she_p[i])); then
        j=$(((i + $1 + 2) % 2))
        READLINE_LINE=${__she_f[j]} READLINE_POINT=${__she_p[j]}
        next=none
      fi
      break
    done
  fi
  if [[ $next == complete ]]; then
    if (($1 < 0)); then
      next=none
    elif [[ "$l|$p" == "$__she_last" ]]; then
      # a second Tab with nothing changed lists the completions, as before
      next=possible-completions
    fi
  fi
  case $next in
    none) bind '"\C-x\C-r": redraw-current-line' ;;
    *) bind "\"\\C-x\\C-r\": $next" ;;
  esac
}
# after the completion: remember the line, for the second Tab
__she_tab_after() { __she_last="$READLINE_LINE|$READLINE_POINT"; }

# Enter: inside the body of she [NAME] <<'D' ... D (the cursor above the last
# line, which is D) a new line; otherwise the line runs as before
__she_enter() {
  local l=$READLINE_LINE p=$READLINE_POINT last d
  local re=$'^[[:space:]]*she([[:space:]]+[^[:space:]]+)?[[:space:]]*<<-?[[:space:]]*([\'"]?)([A-Za-z_][A-Za-z0-9_]*)([\'"]?)\n'
  bind '"\C-x\C-a": accept-line'
  [[ $l =~ $re && ${BASH_REMATCH[2]} == "${BASH_REMATCH[4]}" ]] || return 0
  d=${BASH_REMATCH[3]}
  last=${l##*$'\n'}
  local first=${l%%$'\n'*}
  [[ $last == "$d" ]] && ((p > ${#first} && p <= ${#l} - ${#last} - 1)) || return 0
  # the line the cursor is on, from b to e
  local before=${l:0:p} after=${l:p} b e
  b=${before%$'\n'*}
  b=$((${#b} + 1))
  e=${after%%$'\n'*}
  e=$((p + ${#e}))
  if ((e > b)); then
    # a line with text: a new line after the cursor
    READLINE_LINE=$before$'\n'$after READLINE_POINT=$((p + 1))
    bind '"\C-x\C-a": redraw-current-line'
  else
    # an empty line: drop it and run
    READLINE_LINE=${l:0:b-1}${l:e} READLINE_POINT=$((b - 1))
  fi
}

if [[ $- == *i* ]]; then
  bind -x '"\C-x\C-s": __she_tab 1'
  bind -x '"\C-x\C-b": __she_tab -1'
  bind -x '"\C-x\C-t": __she_tab_after'
  bind -x '"\C-x\C-n": __she_enter'
  bind '"\C-x\C-r": complete'
  bind '"\C-x\C-a": accept-line'
  bind '"\t": "\C-x\C-s\C-x\C-r\C-x\C-t"'
  bind '"\e[Z": "\C-x\C-b\C-x\C-r"'
  bind '"\C-m": "\C-x\C-n\C-x\C-a"'
fi

# Tab inside the quotes after <<<, or on a line of the body of she <<'D' ... D
# (bash gives the whole text, from she, as the line): the completions of the
# text so far, from the server's bash (she --complete); readline puts them in
# and lists them. A lone directory comes as its entries, so readline does not
# close the quote after it.
_she() {
  local left=${COMP_LINE:0:COMP_POINT} head text q name w lead cut x
  local -a c r
  local re=$'^[[:space:]]*she([[:space:]]+([^-[:space:]<>|;&][^[:space:]<>|;&]*))?[[:space:]]*<<-?[[:space:]]*([\'"]?)([A-Za-z_][A-Za-z0-9_]*)([\'"]?)\n'
  if [[ $left =~ $re && ${BASH_REMATCH[3]} == "${BASH_REMATCH[5]}" ]]; then
    # the line of the body the cursor is on; readline's word is its end
    name=${BASH_REMATCH[2]}
    text=${left##*$'\n'}
    w=$2
    cut=$((${#text} - ${#w}))
    ((cut < 0)) && cut=0
    mapfile -t r < <(command she --complete "$name" "$text" 2>/dev/null)
    COMPREPLY=()
    for x in "${r[@]}"; do x=${x%$'\r'}; COMPREPLY+=("${x:cut}"); done
    compopt -o nospace -o noquote 2>/dev/null
    return 0
  fi
  if [[ $left != *'<<<'* ]]; then
    # the first argument: the names of the kept bashes (she -l) and options
    ((COMP_CWORD == 1)) || return 0
    mapfile -t r < <(command she -l 2>/dev/null)
    c=(-l -start -stop -setup)
    for x in "${r[@]}"; do c+=("${x%%$'\t'*}"); done
    mapfile -t COMPREPLY < <(compgen -W "${c[*]}" -- "$2")
    return 0
  fi
  head=${left%%'<<<'*}
  text=${left#*'<<<'}
  text=${text#"${text%%[![:space:]]*}"}
  q=${text:0:1}
  case $q in "'" | '"') text=${text:1} ;; *) q= ;; esac
  [[ -n $q && $text == *"$q"* ]] && return 0
  read -ra c <<< "$head"
  name=${c[1]}
  w=$2
  [[ -n $q && ${w:0:1} == "$q" ]] && { lead=$q; w=${w:1}; }
  cut=$((${#text} - ${#w}))
  ((cut < 0)) && cut=0
  mapfile -t r < <(command she --complete "$name" "$text" 2>/dev/null)
  COMPREPLY=()
  for x in "${r[@]}"; do x=${x%$'\r'}; COMPREPLY+=("$lead${x:cut}"); done
  compopt -o nospace -o noquote 2>/dev/null
}
complete -F _she she

# The first word of a line (a command name) gets bash's own completion, not
# _she, also in the body of she <<'D' ... D: there it goes to _she; anywhere
# else to what completed first words before (complete -I, else bash's own).
__she_prev_I=$(complete -p -I 2>/dev/null)
__she_prev_I=${__she_prev_I#* -F }
[[ $__she_prev_I == complete* ]] && __she_prev_I=
__she_prev_I=${__she_prev_I%% *}
_she_initial() {
  local re=$'^[[:space:]]*she([[:space:]]+[^-[:space:]<>|;&][^[:space:]<>|;&]*)?[[:space:]]*<<-?[[:space:]]*[\'"]?[A-Za-z_][A-Za-z0-9_]*[\'"]?\n'
  if [[ ${COMP_LINE:0:COMP_POINT} =~ $re ]]; then
    _she "$@"
  elif [[ -n $__she_prev_I ]]; then
    "$__she_prev_I" "$@"
  else
    COMPREPLY=()
    compopt -o bashdefault -o default 2>/dev/null
  fi
}
complete -I -F _she_initial 2>/dev/null
