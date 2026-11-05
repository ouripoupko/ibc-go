// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// A poll with one vote per agent. Shows array arguments, several return
/// values, and custom errors.
contract Poll {
    string public question;
    string[] private options;
    uint256[] private votes;
    mapping(address => uint256) private choice; // option index + 1; 0 = has not voted
    uint256 public voters;

    error AlreadyVoted(uint256 option);
    error NoSuchOption(uint256 option, uint256 count);

    constructor(string memory text, string[] memory choices) {
        require(choices.length >= 2, "a poll needs at least two options");
        question = text;
        options = choices;
        votes = new uint256[](choices.length);
    }

    /// Votes for an option, by index. Each agent votes once.
    function vote(uint256 option) external returns (uint256) {
        if (option >= options.length) revert NoSuchOption(option, options.length);
        if (choice[msg.sender] != 0) revert AlreadyVoted(choice[msg.sender] - 1);
        choice[msg.sender] = option + 1;
        votes[option] += 1;
        voters += 1;
        return votes[option];
    }

    /// The question, the options and the votes for each.
    function results() external view returns (string memory, string[] memory, uint256[] memory) {
        return (question, options, votes);
    }

    /// Whether the calling agent has voted, and for which option.
    function myVote() external view returns (bool voted, uint256 option) {
        uint256 c = choice[msg.sender];
        return (c != 0, c == 0 ? 0 : c - 1);
    }
}
