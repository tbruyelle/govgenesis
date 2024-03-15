package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/olekukonko/tablewriter"

	sdk "github.com/cosmos/cosmos-sdk/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
)

// Some constants
var (
	// list of ICF wallets
	icfWallets = []string{
		// Source https://github.com/gnolang/bounties/issues/18#issuecomment-1034700230
		"cosmos1z8mzakma7vnaajysmtkwt4wgjqr2m84tzvyfkz",
		"cosmos1unc788q8md2jymsns24eyhua58palg5kc7cstv",
		// The 2 addresses above have been emptied in favour of the following 2
		"cosmos1sufkm72dw7ua9crpfhhp0dqpyuggtlhdse98e7",
		"cosmos1z6czaavlk6kjd48rpf58kqqw9ssad2uaxnazgl",
	}
	yesVotesMultiplier = sdk.OneDec()               // Y get x1
	noVotesMultiplier  = sdk.NewDec(4)              // N & NWV get 1+x3
	bonus              = sdk.NewDecWithPrec(103, 2) // 3% bonus
	malus              = sdk.NewDecWithPrec(97, 2)  // -3% malus
)

type airdrop struct {
	// addresses contains the airdrop amount per address.
	addresses map[string]sdk.Int
	// blend is the neutral multiplier, for which the $ATOM is neither rewarded
	// nor diluted.
	blend sdk.Dec
	// supply of the airdrop.
	supply sdk.Dec
	// votes holds the part of the airdrop per vote.
	votes voteMap
	// unstaked is part of the airdrop for unstaked amounts.
	unstaked sdk.Dec
}

func distribution(accounts []Account) (airdrop, error) {
	var (
		activeVoteAmts = newVoteMap()
		totalSupply    = sdk.ZeroDec()
	)
	for i := range accounts {
		acc := &accounts[i]
		// init account.votePercs
		acc.votePercs = newVoteMap()
		totalSupply = totalSupply.Add(acc.StakedAmount).Add(acc.LiquidAmount)
		if acc.StakedAmount.IsZero() {
			// No stake, consider non-voter
			acc.votePercs[govtypes.OptionEmpty] = sdk.OneDec()
			continue
		}
		if len(acc.Vote) == 0 {
			// not a direct voter, check for delegated votes
			for _, del := range acc.Delegations {
				// Compute percentage of the delegation over the total staked amount
				delPerc := del.Amount.Quo(acc.StakedAmount)
				if len(del.Vote) == 0 {
					// user didn't vote and delegation didn't either, use the UNSPECIFIED
					// vote option to track it.
					acc.votePercs.add(govtypes.OptionEmpty, delPerc)
				} else {
					for _, vote := range del.Vote {
						acc.votePercs.add(vote.Option, vote.Weight.Mul(delPerc))

						if vote.Option != govtypes.OptionAbstain {
							activeVoteAmts.add(vote.Option, del.Amount.Mul(vote.Weight))
						}
					}
				}
			}
		} else {
			// direct voter
			for _, vote := range acc.Vote {
				acc.votePercs[vote.Option] = vote.Weight

				if vote.Option != govtypes.OptionAbstain {
					activeVoteAmts.add(vote.Option, acc.StakedAmount.Mul(vote.Weight))
				}
			}
		}
	}
	// Compute percentage of Y, N and NWM amouts relative to activeVotesTotalAmt
	activePercs := activeVoteAmts.toPercentages()

	// Compute blend
	blend := activePercs[govtypes.OptionYes].Mul(yesVotesMultiplier).
		Add(activePercs[govtypes.OptionNo].Mul(noVotesMultiplier)).
		Add(activePercs[govtypes.OptionNoWithVeto].Mul(noVotesMultiplier))

	// Now that blend is computed, loop again on accounts and apply the multipliers.
	icfSlash := sdk.ZeroDec()
	airdrop := airdrop{
		addresses: make(map[string]sdk.Int),
		blend:     blend,
		supply:    sdk.ZeroDec(),
		votes:     newVoteMap(),
		unstaked:  sdk.ZeroDec(),
	}
	for _, acc := range accounts {
		if slices.Contains(icfWallets, acc.Address) {
			// Slash ICF
			icfSlash = icfSlash.Add(acc.LiquidAmount).Add(acc.StakedAmount)
			continue
		}
		var (
			// stakingMultiplier details:
			// Yes:         x yesVotesMultiplier
			// No:         	x noVotesMultiplier
			// NoWithVeto: 	x noVotesMultiplier x bonus
			// Abstain:    	x blend
			// Didn't vote: x blend x malus
			yesAirdropAmt        = acc.votePercs[govtypes.OptionYes].Mul(yesVotesMultiplier).Mul(acc.StakedAmount)
			noAirdropAmt         = acc.votePercs[govtypes.OptionNo].Mul(noVotesMultiplier).Mul(acc.StakedAmount)
			noWithVetoAirdropAmt = acc.votePercs[govtypes.OptionNoWithVeto].Mul(noVotesMultiplier).Mul(bonus).Mul(acc.StakedAmount)
			abstainAirdropAmt    = acc.votePercs[govtypes.OptionAbstain].Mul(blend).Mul(acc.StakedAmount)
			noVoteAirdropAmt     = acc.votePercs[govtypes.OptionEmpty].Mul(blend).Mul(malus).Mul(acc.StakedAmount)

			// Liquid amount gets the same multiplier as those who didn't vote.
			liquidMultiplier = blend.Mul(malus)

			// total airdrop for this account
			airdropAmt = acc.LiquidAmount.Mul(liquidMultiplier).
					Add(yesAirdropAmt).Add(noAirdropAmt).Add(noWithVetoAirdropAmt).
					Add(abstainAirdropAmt).Add(noVoteAirdropAmt)
		)
		// increment airdrop votes
		airdrop.votes.add(govtypes.OptionYes, yesAirdropAmt)
		airdrop.votes.add(govtypes.OptionNo, noAirdropAmt)
		airdrop.votes.add(govtypes.OptionNoWithVeto, noWithVetoAirdropAmt)
		airdrop.votes.add(govtypes.OptionAbstain, abstainAirdropAmt)
		airdrop.votes.add(govtypes.OptionEmpty, noVoteAirdropAmt)
		// increment airdrop supply
		airdrop.supply = airdrop.supply.Add(airdropAmt)
		// add addresse and amount
		airdrop.addresses[acc.Address] = airdropAmt.TruncateInt()
	}

	var (
		totalDidntVoteAirdrop  = airdrop.votes[govtypes.OptionEmpty]
		totalYesAirdrop        = airdrop.votes[govtypes.OptionYes]
		totalNoAirdrop         = airdrop.votes[govtypes.OptionNo]
		totalNoWithVetoAirdrop = airdrop.votes[govtypes.OptionNoWithVeto]
		totalAbstainAirdrop    = airdrop.votes[govtypes.OptionAbstain]
		totalStakedAirdrop     = totalDidntVoteAirdrop.Add(totalYesAirdrop).
					Add(totalNoAirdrop).Add(totalNoWithVetoAirdrop).Add(totalAbstainAirdrop)
	)
	airdrop.unstaked = airdrop.supply.Sub(totalStakedAirdrop)

	fmt.Println("BLEND", blend)
	fmt.Println("TOTAL SUPPLY ", humand(totalSupply))
	fmt.Println("TOTAL AIRDROP", humand(airdrop.supply))
	fmt.Println("RATIO", airdrop.supply.Quo(totalSupply))
	fmt.Println("RELATIVE PERCS", activePercs)
	fmt.Println("ICF SLASH", humand(icfSlash))

	return airdrop, nil
}

// convienient type for manipulating vote counts.
type voteMap map[govtypes.VoteOption]sdk.Dec

var (
	allVoteOptions = []govtypes.VoteOption{
		govtypes.OptionEmpty,
		govtypes.OptionYes,
		govtypes.OptionAbstain,
		govtypes.OptionNo,
		govtypes.OptionNoWithVeto,
	}
	activeVoteOptions = []govtypes.VoteOption{
		govtypes.OptionYes,
		govtypes.OptionNo,
		govtypes.OptionNoWithVeto,
	}
)

func newVoteMap() voteMap {
	m := make(map[govtypes.VoteOption]sdk.Dec)
	for _, v := range allVoteOptions {
		m[v] = sdk.ZeroDec()
	}
	return m
}

func (m voteMap) add(v govtypes.VoteOption, d sdk.Dec) {
	m[v] = m[v].Add(d)
}

func (m voteMap) total() sdk.Dec {
	d := sdk.ZeroDec()
	for _, v := range m {
		d = d.Add(v)
	}
	return d
}

func (m voteMap) toPercentages() map[govtypes.VoteOption]sdk.Dec {
	total := m.total()
	percs := make(map[govtypes.VoteOption]sdk.Dec)
	for k, v := range m {
		percs[k] = v.Quo(total)
	}
	return percs
}

func printAirdropStats(a airdrop) {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"", "TOTAL", "DID NOT VOTE", "YES", "NO", "NOWITHVETO", "ABSTAIN", "NOT STAKED"})
	table.Append([]string{
		"Distributed $ATONE",
		humand(a.supply),
		humand(a.votes[govtypes.OptionEmpty]),
		humand(a.votes[govtypes.OptionYes]),
		humand(a.votes[govtypes.OptionNo]),
		humand(a.votes[govtypes.OptionNoWithVeto]),
		humand(a.votes[govtypes.OptionAbstain]),
		humand(a.unstaked),
	})
	table.Append([]string{
		"Percentage over total",
		"",
		humanPercent(a.votes[govtypes.OptionEmpty].Quo(a.supply)),
		humanPercent(a.votes[govtypes.OptionYes].Quo(a.supply)),
		humanPercent(a.votes[govtypes.OptionNo].Quo(a.supply)),
		humanPercent(a.votes[govtypes.OptionNoWithVeto].Quo(a.supply)),
		humanPercent(a.votes[govtypes.OptionAbstain].Quo(a.supply)),
		humanPercent(a.unstaked.Quo(a.supply)),
	})
	table.Render()
}
